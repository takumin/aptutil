package mirror

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
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
