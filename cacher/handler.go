package cacher

import (
	"fmt"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cybozu-go/log"
)

type cacheHandler struct {
	*Cacher
}

func (c cacheHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET", "HEAD":
		// later on
	default:
		http.Error(w, "bad method", http.StatusNotImplemented)
		return
	}

	// r.URL.Path is empty for a request such as "GET http://host HTTP/1.1".
	p := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))

	if log.Enabled(log.LvDebug) {
		log.Debug("request path", map[string]interface{}{
			"path": p,
		})
	}

	if r.Method == "HEAD" {
		c.serveHead(w, r, p)
		return
	}

	status, f, err := c.Get(r.Context(), p)
	if !writeError(w, r, status, err) {
		return
	}

	// http.StatusOK
	defer func() { _ = f.Close() }()
	var zeroTime time.Time
	http.ServeContent(w, r, path.Base(p), zeroTime, f)
}

// serveHead responds to a HEAD request without downloading the item.
func (c cacheHandler) serveHead(w http.ResponseWriter, r *http.Request, p string) {
	status, size, err := c.Head(r.Context(), p)
	if !writeError(w, r, status, err) {
		return
	}

	// http.StatusOK
	ct := mime.TypeByExtension(path.Ext(p))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
}

// writeError writes an error response unless status is http.StatusOK
// and err is nil.  It returns true if nothing is written.
func writeError(w http.ResponseWriter, r *http.Request, status int, err error) bool {
	switch {
	case err != nil:
		// do not expose internal errors such as file paths to clients.
		http.Error(w, http.StatusText(status), status)
	case status == http.StatusNotFound:
		http.NotFound(w, r)
	case status != http.StatusOK:
		http.Error(w, fmt.Sprintf("status %d", status), status)
	default:
		return true
	}
	return false
}
