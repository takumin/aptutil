package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybozu-go/aptutil/apt"
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
		id:           "test",
		mc:           &MirrConfig{URL: tomlURL{u}},
		storage:      storage,
		semaphore:    make(chan struct{}, 1),
		client:       &http.Client{Transport: transport},
		stallTimeout: stallTimeout,
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

	srv := serveRepo(t, map[string]string{"dists/s/InRelease": "Origin: test\n" + releaseOf(map[string]string{"README": "readme"})})

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
	if _, ok := indexMap["dists/s/README"]; !ok || len(indexMap) != 1 {
		t.Errorf("indexMap = %v, want dists/s/README only", indexMap)
	}
}

func TestMirrorDownloadRetriesStalled(t *testing.T) {
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

			var reqs atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if reqs.Add(1) == 1 {
					// keep the connection open without sending the rest.
					tc.stall(w)
					<-r.Context().Done()
					return
				}
				_, _ = w.Write([]byte("data"))
			}))
			defer srv.Close()

			m := newTestMirror(t, srv.URL)
			m.stallTimeout = 100 * time.Millisecond
			ch := make(chan *dlResult, 1)
			go m.download(context.Background(), "a", nil, false, ch)

			select {
			case r := <-ch:
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
			case <-time.After(10 * time.Second):
				t.Fatal("download did not return")
			}
		})
	}
}

func TestMirrorDownloadSlowButProgressing(t *testing.T) {
	t.Parallel()

	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		w.Header().Set("Content-Length", "8")
		// the whole body takes longer than stallTimeout, but each
		// chunk arrives well within it so that delays in scheduling
		// do not make it look stalled.
		for _, b := range []byte("datadata") {
			_, _ = w.Write([]byte{b})
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	m := newTestMirror(t, srv.URL)
	m.stallTimeout = 500 * time.Millisecond
	ch := make(chan *dlResult, 1)
	m.download(context.Background(), "a", nil, false, ch)
	r := <-ch
	if r.tempfile != nil {
		closeAndRemoveFile(r.tempfile)
	}

	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.fi.Size() != 8 {
		t.Errorf("size = %d, want 8", r.fi.Size())
	}
	if n := reqs.Load(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestMirrorDownloadCancelAfterStall(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestMirror(t, srv.URL)
	m.stallTimeout = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan *dlResult, 1)
	go m.download(ctx, "a", nil, false, ch)

	// retries back off for 1+2+4+8+16 seconds in total; cancel the
	// parent context once a stall has been observed to keep this
	// test short, and check the error is not misreported as a stall.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case r := <-ch:
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not return")
	}
}

func TestMirrorDownloadLocalErrorNotRetried(t *testing.T) {
	t.Parallel()

	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	m := newTestMirror(t, srv.URL)
	// make TempFile fail.
	if err := os.RemoveAll(m.storage.Dir()); err != nil {
		t.Fatal(err)
	}

	ch := make(chan *dlResult, 1)
	m.download(context.Background(), "a", nil, false, ch)
	r := <-ch

	var lerr *localError
	if !errors.As(r.err, &lerr) {
		t.Errorf("err = %v, want localError", r.err)
	}
	if n := reqs.Load(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestMirrorDownloadReleaseWaitsForDownloads(t *testing.T) {
	t.Parallel()

	files := flatRepo(map[string]string{"a.deb": "a"})
	// InRelease of the next generation listing b.deb.
	next := flatRepo(map[string]string{"a.deb": "a", "b.deb": "b"})
	files["InRelease"] = next["Release"]

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "Release.gpg" {
			// respond after downloadRelease fails for the
			// inconsistent Release and InRelease.
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		data, ok := files[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(data))
	}))
	defer srv.Close()

	m := newTestMirror(t, srv.URL)
	const slots = 8
	m.semaphore = make(chan struct{}, slots)
	for range slots {
		m.semaphore <- struct{}{}
	}

	_, _, err := m.downloadRelease(context.Background(), "/")
	if err == nil {
		t.Error("downloadRelease must fail")
	}
	close(release)

	// a download left running would save Release.gpg in a temporary
	// file that nobody removes.
	deadline := time.Now().Add(5 * time.Second)
	for len(m.semaphore) < slots {
		if time.Now().After(deadline) {
			t.Fatal("downloads did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	l, err := filepath.Glob(filepath.Join(m.storage.dir, "_tmp*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 0 {
		t.Errorf("temporary files are left: %v", l)
	}
}

func TestMirrorDownloadLocalFailureStopsDownload(t *testing.T) {
	t.Parallel()

	const total = 256 << 20
	var sent atomic.Int64
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(total))
		for sent.Load() < total {
			n, err := w.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	m := newTestMirror(t, srv.URL)

	// removing the storage directory makes TempFile fail.
	if err := os.RemoveAll(m.storage.Dir()); err != nil {
		t.Fatal(err)
	}
	ch := make(chan *dlResult, 1)
	m.download(context.Background(), "a.deb", nil, false, ch)
	r := <-ch
	var lerr *localError
	if !errors.As(r.err, &lerr) {
		t.Errorf("err = %v, want a local error", r.err)
	}
	if n := sent.Load(); n > total/4 {
		t.Errorf("upstream sent %d MiB of %d MiB after a local failure", n>>20, total>>20)
	}
}

// TestMirrorDownloadFilesReuseFailure tests that a failure to reuse an
// item cancels downloads in progress instead of waiting for them.
func TestMirrorDownloadFilesReuseFailure(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Second):
		}
		_, _ = w.Write([]byte("a"))
	}))
	defer srv.Close()

	m := newTestMirror(t, srv.URL)
	cur, err := NewStorage(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	store := func(p string) *apt.FileInfo {
		fi, err := makeFileInfo(p, []byte(p))
		if err != nil {
			t.Fatal(err)
		}
		f, err := cur.TempFile()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := cur.StoreLink(fi, f.Name()); err != nil {
			t.Fatal(err)
		}
		return fi
	}
	fa, err := makeFileInfo("y/a.deb", []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	// items reused successfully delay the failure to reuse x/b.deb so
	// that the download of y/a.deb starts before it.
	fil := []*apt.FileInfo{fa}
	for i := range 1000 {
		fil = append(fil, store(fmt.Sprintf("z/%d.deb", i)))
	}
	fil = append(fil, store("x/b.deb"))
	m.current = cur
	m.semaphore = nil

	// a regular file at the directory of x/b.deb makes reusing it fail.
	if err := os.MkdirAll(filepath.Join(m.storage.Dir(), "test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.storage.Dir(), "test", "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = m.downloadFiles(context.Background(), fil, false, false)
	if err == nil || !strings.Contains(err.Error(), "storeLink") {
		t.Fatalf("downloadFiles = %v, want the failure to reuse", err)
	}
	if hits.Load() == 0 {
		t.Skip("the download did not start before the failure")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("downloadFiles returned after %v", d)
	}
}
