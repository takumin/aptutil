package cacher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybozu-go/aptutil/apt"
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
		status, f, err := c.Get(context.Background(), p)
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

func TestCacherUpdateListed(t *testing.T) {
	t.Parallel()

	c := newTestCacher(t, "http://example.com")

	mustFI := func(p, data string) *apt.FileInfo {
		t.Helper()
		fi, err := apt.CopyWithFileInfo(new(bytes.Buffer), bytes.NewReader([]byte(data)), p)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}

	const (
		gz = "ubuntu/dists/noble/main/binary-amd64/Packages.gz"
		xz = "ubuntu/dists/noble/main/binary-amd64/Packages.xz"
		a  = "ubuntu/pool/a.deb"
		b  = "ubuntu/pool/b.deb"
		c2 = "ubuntu/pool/c.deb"
	)

	c.updateListed(gz, []*apt.FileInfo{mustFI(a, "a1"), mustFI(b, "b1")}, nil)
	c.updateListed(xz, []*apt.FileInfo{mustFI(a, "a1"), mustFI(b, "b1")}, nil)

	// Packages.gz is updated: a is replaced by c, and b is updated.
	newB := mustFI(b, "b2")
	c.updateListed(gz, []*apt.FileInfo{newB, mustFI(c2, "c1")}, nil)

	for _, p := range []string{a, b, c2} {
		if _, ok := c.info[p]; !ok {
			t.Errorf("%s should be kept", p)
		}
	}
	if !c.info[b].Same(newB) {
		t.Errorf("%s should be updated", b)
	}

	// Packages.xz is updated as well; a is no longer listed anywhere.
	c.updateListed(xz, []*apt.FileInfo{newB, mustFI(c2, "c1")}, nil)

	if _, ok := c.info[a]; ok {
		t.Errorf("%s should be removed", a)
	}
	for _, p := range []string{b, c2} {
		if _, ok := c.info[p]; !ok {
			t.Errorf("%s should be kept", p)
		}
	}
	if n := c.refs[b]; n != 2 {
		t.Errorf("refs[%s] = %d, want 2", b, n)
	}

	// an empty list releases all items.
	c.updateListed(gz, nil, nil)
	c.updateListed(xz, nil, nil)
	if len(c.refs) != 0 || len(c.listed) != 0 {
		t.Errorf("refs = %v, listed = %v, want empty", c.refs, c.listed)
	}
	for _, p := range []string{a, b, c2} {
		if _, ok := c.info[p]; ok {
			t.Errorf("%s should be removed", p)
		}
	}
}

func TestNewCacherNegativeMaxConns(t *testing.T) {
	t.Parallel()

	config := NewConfig()
	config.MetaDirectory = t.TempDir()
	config.CacheDirectory = t.TempDir()
	config.MaxConns = -1
	if _, err := NewCacher(config); err == nil {
		t.Error("NewCacher must fail with negative max_conns")
	}
}

func TestCacherMaintReleaseUnmapped(t *testing.T) {
	t.Parallel()

	c := newTestCacher(t, "http://example.com")
	c.checkInterval = 10 * time.Millisecond

	done := make(chan struct{})
	go func() {
		// the prefix "gone" was removed from the mapping.
		c.maintRelease(context.Background(), "gone/dists/noble/InRelease", false)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("maintRelease did not stop for an unmapped path")
	}
}

func TestCacherResultInvalidation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	c := newTestCacher(t, srv.URL)
	c.cachePeriod = 300 * time.Millisecond

	const p = "ubuntu/pool/a.deb"
	<-c.Download(p, nil)
	time.Sleep(150 * time.Millisecond)
	<-c.Download(p, nil)

	// the result of the first download expires, but the second does not.
	time.Sleep(200 * time.Millisecond)
	c.dlLock.Lock()
	_, ok := c.results[p]
	c.dlLock.Unlock()
	if !ok {
		t.Error("the result of the newer download was invalidated")
	}
}

func TestCacherGetInsertFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	c := newTestCacher(t, srv.URL)

	// a regular file where a directory is needed makes Insert fail.
	if err := os.WriteFile(filepath.Join(c.items.dir, "ubuntu"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	status, f, err := c.Get(context.Background(), "ubuntu/pool/a.deb")
	if f != nil {
		_ = f.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", status, http.StatusInternalServerError)
	}
}

func TestCacherGetCanceled(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()

	c := newTestCacher(t, srv.URL)

	const p = "ubuntu/pool/a.deb"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, f, err := c.Get(ctx, p)
		if f != nil {
			_ = f.Close()
		}
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
		t.Fatal("Get did not return after cancel")
	}

	// the download continues; wait for it before removing the directories.
	c.dlLock.Lock()
	task := c.dlTasks[p]
	c.dlLock.Unlock()
	close(release)
	if task != nil {
		<-task.done
	}
}

// tempFiles returns the temporary files left in dir.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()

	l, err := filepath.Glob(filepath.Join(dir, "_tmp*"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestCacherGetTooLarge(t *testing.T) {
	t.Parallel()

	const body = "0123456789"
	var hits atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newTestCacher(t, srv.URL)
	c.items.capacity = 4 // smaller than the item

	const p = "ubuntu/pool/big.deb"
	type result struct {
		status int
		data   string
		err    error
	}
	const waiters = 2
	done := make(chan result, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			status, f, err := c.Get(context.Background(), p)
			if f == nil {
				done <- result{status, "", err}
				return
			}
			defer func() { _ = f.Close() }()
			data, err := io.ReadAll(f)
			done <- result{status, string(data), err}
		}()
	}

	// let both waiters join the same download.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.dlLock.Lock()
		task := c.dlTasks[p]
		joined := task != nil && task.refs == waiters+1
		c.dlLock.Unlock()
		if joined {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiters did not join the download")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)

	for i := 0; i < waiters; i++ {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.status != http.StatusOK || r.data != body {
				t.Errorf("status = %d, data = %q, want %d, %q", r.status, r.data, http.StatusOK, body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Get did not return")
		}
	}

	if n := hits.Load(); n != 1 {
		t.Errorf("upstream hits = %d, want 1", n)
	}
	if n := c.items.Len(); n != 0 {
		t.Errorf("cached items = %d, want 0", n)
	}
	if l := tempFiles(t, c.items.dir); len(l) != 0 {
		t.Errorf("temporary files are left: %v", l)
	}
}

func TestCacherGetUnlisted(t *testing.T) {
	t.Parallel()

	tr := newTestRepo()
	const p = "pool/b.deb"
	tr.set(p, []byte("b"))
	srv := httptest.NewServer(tr)
	defer srv.Close()

	c := newTestCacher(t, srv.URL)

	for i := 0; i < 2; i++ {
		if status, data := getData(t, c, "ubuntu/"+p); status != http.StatusOK || data != "b" {
			t.Fatalf("Get = %d, %q", status, data)
		}
	}
	if n := tr.hits(p); n != 1 {
		t.Errorf("upstream hits = %d, want 1", n)
	}

	// items not listed in meta data are looked up in Storage.
	if _, ok := c.info["ubuntu/"+p]; ok {
		t.Error("an unlisted item should not be kept in info")
	}

	// cached items are served after restart without downloading.
	c2, err := NewCacher(&Config{
		CheckInterval:  defaultCheckInterval,
		CachePeriod:    defaultCachePeriod,
		MetaDirectory:  c.meta.dir,
		CacheDirectory: c.items.dir,
		CacheCapacity:  defaultCacheCapacity,
		Mapping:        map[string]string{"ubuntu": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, data := getData(t, c2, "ubuntu/"+p); status != http.StatusOK || data != "b" {
		t.Fatalf("Get after restart = %d, %q", status, data)
	}
	if n := tr.hits(p); n != 1 {
		t.Errorf("upstream hits after restart = %d, want 1", n)
	}
}
