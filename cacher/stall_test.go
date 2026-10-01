package cacher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacherGetStalled(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		stall func(w http.ResponseWriter)
	}{
		{
			name:  "header",
			stall: func(w http.ResponseWriter) {},
		},
		{
			name: "body",
			stall: func(w http.ResponseWriter) {
				w.Header().Set("Content-Length", "4")
				_, _ = w.Write([]byte("da"))
				w.(http.Flusher).Flush()
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// keep the connection open without sending the rest.
				tc.stall(w)
				<-r.Context().Done()
			}))
			defer srv.Close()

			c := newTestCacher(t, srv.URL)
			c.stallTimeout = 100 * time.Millisecond

			type result struct {
				status int
				err    error
			}
			done := make(chan result, 1)
			go func() {
				status, f, err := c.Get(context.Background(), "ubuntu/pool/a.deb")
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
				if r.status != http.StatusGatewayTimeout {
					t.Errorf("status = %d, want %d", r.status, http.StatusGatewayTimeout)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Get did not return")
			}

			if l := tempFiles(t, c.items.dir); len(l) != 0 {
				t.Errorf("temporary files are left: %v", l)
			}
		})
	}
}

func TestCacherGetSlowButProgressing(t *testing.T) {
	t.Parallel()

	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		w.Header().Set("Content-Length", "4")
		// the whole body takes longer than stallTimeout, but each
		// chunk arrives within it.
		for _, b := range []byte("data") {
			_, _ = w.Write([]byte{b})
			w.(http.Flusher).Flush()
			time.Sleep(60 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := newTestCacher(t, srv.URL)
	c.stallTimeout = 150 * time.Millisecond

	status, f, err := c.Get(context.Background(), "ubuntu/pool/a.deb")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "data" {
		t.Errorf("data = %q, want %q", data, "data")
	}
	if n := reqs.Load(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestCacherHeadStalled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := newTestCacher(t, srv.URL)
	c.stallTimeout = 100 * time.Millisecond

	status, _, err := c.Head(context.Background(), "ubuntu/pool/a.deb")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want %d", status, http.StatusGatewayTimeout)
	}
}

func TestCacherHeadCanceledWhileWaitingForConnection(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	c := newTestCacher(t, srv.URL)
	c.maxConns = 1

	// occupy the only connection to the upstream.
	host := c.um.URL("ubuntu/pool/a.deb").Host
	if err := c.acquireSemaphore(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	defer c.releaseSemaphore(host)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Head(ctx, "ubuntu/pool/a.deb")
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Head did not return after cancel")
	}
}
