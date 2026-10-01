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

func TestCacheHandlerHead(t *testing.T) {
	t.Parallel()

	tr := newTestRepo()
	tr.set("pool/a.deb", []byte("aaaa"))
	tr.set("pool/b.deb", []byte("bb"))
	srv := httptest.NewServer(tr)
	defer srv.Close()

	c := newTestCacher(t, srv.URL)
	if status, _ := getData(t, c, "ubuntu/pool/a.deb"); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	cases := []struct {
		path   string
		status int
		length string
	}{
		{"/ubuntu/pool/a.deb", http.StatusOK, "4"},
		{"/ubuntu/pool/b.deb", http.StatusOK, "2"},
		{"/ubuntu/pool/c.deb", http.StatusNotFound, ""},
		{"/unknown/pool/a.deb", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		cacheHandler{c}.ServeHTTP(w, httptest.NewRequest(http.MethodHead, tc.path, nil))
		if w.Code != tc.status {
			t.Errorf("HEAD %s: status = %d, want %d", tc.path, w.Code, tc.status)
			continue
		}
		if tc.length != "" && w.Header().Get("Content-Length") != tc.length {
			t.Errorf("HEAD %s: Content-Length = %q, want %q",
				tc.path, w.Header().Get("Content-Length"), tc.length)
		}
	}

	// cached items are answered locally, and uncached ones are not downloaded.
	if n := tr.heads.Load(); n != 2 {
		t.Errorf("upstream HEAD requests = %d, want 2", n)
	}
	for _, p := range []string{"pool/b.deb", "pool/c.deb"} {
		if n := tr.hits(p); n != 0 {
			t.Errorf("%s was downloaded %d times, want 0", p, n)
		}
	}
	if n := c.items.Len(); n != 1 {
		t.Errorf("cached items = %d, want 1", n)
	}
}
