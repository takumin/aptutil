package mirror

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func newTestMirror(t *testing.T, upstream string) *Mirror {
	t.Helper()

	u, err := url.Parse(upstream + "/")
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewStorage(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	// use a dedicated transport; httptest.Server.Close closes idle
	// connections of http.DefaultTransport shared by parallel tests.
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	return &Mirror{
		id:        "test",
		mc:        &MirrConfig{URL: tomlURL{u}},
		storage:   storage,
		semaphore: make(chan struct{}, 1),
		client:    &http.Client{Transport: transport},
	}
}

func TestMirrorDownloadReusesConnectionOnRetry(t *testing.T) {
	t.Parallel()

	var reqs, conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) == 1 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("data"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	m := newTestMirror(t, srv.URL)
	ch := make(chan *dlResult, 1)
	m.download(context.Background(), "a", nil, false, ch)
	r := <-ch
	if r.tempfile != nil {
		closeAndRemoveFile(r.tempfile)
	}

	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.status != http.StatusOK {
		t.Errorf("status = %d, want %d", r.status, http.StatusOK)
	}
	if n := reqs.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
	// the response for the failed request must be closed before retrying
	// so that the connection is reused.
	if n := conns.Load(); n != 1 {
		t.Errorf("connections = %d, want 1", n)
	}
}

func TestMirrorDownloadCancelDuringBackoff(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	m := newTestMirror(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan *dlResult, 1)
	go m.download(ctx, "a", nil, false, ch)

	// the first retry waits for 1 second; cancel while waiting.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case r := <-ch:
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Errorf("download returned %v after cancel", elapsed)
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not return")
	}
}

func TestMirrorUnlimitedConns(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{
		Dir:      t.TempDir(),
		MaxConns: 0, // no limit
		Mirrors: map[string]*MirrConfig{
			"test": {URL: tomlURL{u}, Suites: []string{"s"}},
		},
	}
	m, err := NewMirror(time.Now(), "test", c)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	indexMap, _, err := m.downloadRelease(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(indexMap) != 0 {
		t.Errorf("indexMap = %v, want empty", indexMap)
	}
}
