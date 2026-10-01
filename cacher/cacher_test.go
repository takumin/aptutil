package cacher

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestCacher(t *testing.T, upstream string) *Cacher {
	t.Helper()

	config := NewConfig()
	config.MetaDirectory = t.TempDir()
	config.CacheDirectory = t.TempDir()
	config.Mapping = map[string]string{"ubuntu": upstream}
	c, err := NewCacher(config)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCacherGetChecksumMismatch(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("unexpected"))
	}))
	defer srv.Close()

	c := newTestCacher(t, srv.URL)

	const p = "ubuntu/pool/a.deb"
	fi, err := makeFileInfo(p, []byte("expected"))
	if err != nil {
		t.Fatal(err)
	}
	c.info[p] = fi

	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, f, err := c.Get(p)
		if f != nil {
			_ = f.Close()
		}
		done <- result{status, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.status != http.StatusBadGateway {
			t.Errorf("status = %d, want %d", r.status, http.StatusBadGateway)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get did not return")
	}

	if n := hits.Load(); n != 1 {
		t.Errorf("upstream hits = %d, want 1", n)
	}
}

func TestNewTransport(t *testing.T) {
	t.Parallel()

	if n := newTransport(10).MaxIdleConnsPerHost; n != 10 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 10", n)
	}

	// zero means no limit on connections; keep the default.
	if n := newTransport(0).MaxIdleConnsPerHost; n != 0 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 0", n)
	}
}
