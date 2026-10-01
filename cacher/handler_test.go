package cacher

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheHandlerEmptyPath(t *testing.T) {
	t.Parallel()

	c := newTestCacher(t, "http://example.com")

	// the request line "GET http://example.com HTTP/1.1" has no path.
	r := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	r.URL.Path = ""
	w := httptest.NewRecorder()
	cacheHandler{c}.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestCacheHandlerHidesInternalErrors(t *testing.T) {
	t.Parallel()

	config := NewConfig()
	config.MetaDirectory = t.TempDir()
	config.CacheDirectory = t.TempDir()
	config.Mapping = map[string]string{"ubuntu": "http://example.com"}

	const p = "ubuntu/pool/a.deb"
	fp := filepath.Join(config.CacheDirectory, p+fileSuffix)
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fp, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewCacher(config)
	if err != nil {
		t.Fatal(err)
	}

	// removing the loaded file makes Storage.Lookup fail.
	if err := os.Remove(fp); err != nil {
		t.Fatal(err)
	}
	fi, err := makeFileInfo(p, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	c.info[p] = fi

	w := httptest.NewRecorder()
	cacheHandler{c}.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+p, nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if body := w.Body.String(); strings.Contains(body, config.CacheDirectory) {
		t.Errorf("response body exposes an internal path: %q", body)
	}
}
