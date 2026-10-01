package cacher

import (
	"net/http"
	"net/http/httptest"
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
