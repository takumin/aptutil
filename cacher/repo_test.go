package cacher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
)

// testRepo is an upstream APT repository serving files by path.
type testRepo struct {
	mu    sync.Mutex
	files map[string][]byte
	gets  map[string]int
}

func newTestRepo() *testRepo {
	return &testRepo{
		files: make(map[string][]byte),
		gets:  make(map[string]int),
	}
}

func (tr *testRepo) set(p string, data []byte) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.files[p] = data
}

func (tr *testRepo) hits(p string) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.gets[p]
}

func (tr *testRepo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path[1:]
	tr.mu.Lock()
	if r.Method == http.MethodGet {
		tr.gets[p]++
	}
	data, ok := tr.files[p]
	tr.mu.Unlock()

	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

// getData calls c.Get and returns the status and the content.
func getData(t *testing.T, c *Cacher, p string) (int, string) {
	t.Helper()

	status, f, err := c.Get(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		return status, ""
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return status, string(data)
}
