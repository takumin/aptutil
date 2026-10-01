package cacher

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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

	c.updateListed(gz, []*apt.FileInfo{mustFI(a, "a1"), mustFI(b, "b1")})
	c.updateListed(xz, []*apt.FileInfo{mustFI(a, "a1"), mustFI(b, "b1")})

	// Packages.gz is updated: a is replaced by c, and b is updated.
	newB := mustFI(b, "b2")
	c.updateListed(gz, []*apt.FileInfo{newB, mustFI(c2, "c1")})

	for _, p := range []string{a, b, c2} {
		if _, ok := c.info[p]; !ok {
			t.Errorf("%s should be kept", p)
		}
	}
	if !c.info[b].Same(newB) {
		t.Errorf("%s should be updated", b)
	}

	// Packages.xz is updated as well; a is no longer listed anywhere.
	c.updateListed(xz, []*apt.FileInfo{newB, mustFI(c2, "c1")})

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
	c.updateListed(gz, nil)
	c.updateListed(xz, nil)
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
