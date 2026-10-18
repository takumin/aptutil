package cacher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
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

func TestNewCacherNonPositiveCheckInterval(t *testing.T) {
	t.Parallel()

	for _, interval := range []int{0, -1} {
		config := NewConfig()
		config.MetaDirectory = t.TempDir()
		config.CacheDirectory = t.TempDir()
		config.CheckInterval = interval
		if _, err := NewCacher(config); err == nil {
			t.Errorf("NewCacher must fail with check_interval = %d", interval)
		}
	}
}

func TestNewCacherOverlappingDirectories(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testCases := []struct {
		name        string
		meta, cache string
		wantErr     bool
	}{
		{name: "same", meta: dir, cache: dir, wantErr: true},
		{name: "meta in cache", meta: filepath.Join(dir, "meta"), cache: dir, wantErr: true},
		{name: "cache in meta", meta: dir, cache: filepath.Join(dir, "cache"), wantErr: true},
		{name: "siblings", meta: filepath.Join(dir, "meta"), cache: filepath.Join(dir, "meta-cache")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			config := NewConfig()
			config.MetaDirectory = tc.meta
			config.CacheDirectory = tc.cache
			_, err := NewCacher(config)
			if tc.wantErr && err == nil {
				t.Error("NewCacher must fail")
			}
			if !tc.wantErr && err != nil {
				t.Error(err)
			}
		})
	}
}

func TestNewCacherNegativeCachePeriod(t *testing.T) {
	t.Parallel()

	config := NewConfig()
	config.MetaDirectory = t.TempDir()
	config.CacheDirectory = t.TempDir()
	config.CachePeriod = -1
	if _, err := NewCacher(config); err == nil {
		t.Error("NewCacher must fail with negative cache_period")
	}
}

// TestCacherGetNoCachePeriod tests that Get returns the result of the
// download it waited for even if the result has already expired.
func TestCacherGetNoCachePeriod(t *testing.T) {
	t.Parallel()

	tr := newTestRepo()
	srv := httptest.NewServer(tr)
	defer srv.Close()

	c := newTestCacher(t, srv.URL)
	c.cachePeriod = 0

	const p = "pool/a.deb"
	if status, _ := getData(t, c, "ubuntu/"+p); status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
	if n := tr.hits(p); n != 1 {
		t.Errorf("upstream hits = %d, want 1", n)
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
	// results are expired by expireResult below instead of timers so
	// that the test does not depend on delays in scheduling.
	c.cachePeriod = time.Hour

	const p = "ubuntu/pool/a.deb"
	result := func() *dlStatus {
		c.dlLock.Lock()
		defer c.dlLock.Unlock()
		return c.results[p]
	}

	<-c.Download(p, nil)
	first := result()
	<-c.Download(p, nil)
	second := result()
	if first == nil || second == nil || first == second {
		t.Fatalf("results = %p, %p, want two different results", first, second)
	}

	// the result of the first download expires, but the second does not.
	c.expireResult(p, first)
	if result() != second {
		t.Error("the result of the newer download was invalidated")
	}
	c.expireResult(p, second)
	if r := result(); r != nil {
		t.Errorf("the result was not invalidated: %v", r)
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

	// the downloading goroutine releases the task, and removes the
	// file, after the waiters are woken up.
	deadline = time.Now().Add(5 * time.Second)
	for {
		l := tempFiles(t, c.items.dir)
		if len(l) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporary files are left: %v", l)
		}
		time.Sleep(10 * time.Millisecond)
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

func TestCacherRestartWithBrokenMeta(t *testing.T) {
	t.Parallel()

	tr := newTestRepo()
	const p = "dists/stable/InRelease"
	tr.set(p, []byte("<!DOCTYPE html>\n<html></html>\n"))
	srv := httptest.NewServer(tr)
	defer srv.Close()

	c := newTestCacher(t, srv.URL)

	// broken meta data are accepted and cached as is.
	if status, _ := getData(t, c, "ubuntu/"+p); status != http.StatusOK {
		t.Fatalf("Get = %d", status)
	}

	// the cacher starts even with the broken meta data, and serves them.
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
	if status, _ := getData(t, c2, "ubuntu/"+p); status != http.StatusOK {
		t.Fatalf("Get after restart = %d", status)
	}
	if n := tr.hits(p); n != 1 {
		t.Errorf("upstream hits = %d, want 1", n)
	}
}

func TestCacherRestartWithMetaWithoutPrefix(t *testing.T) {
	t.Parallel()

	config := NewConfig()
	config.MetaDirectory = t.TempDir()
	config.CacheDirectory = t.TempDir()
	config.Mapping = map[string]string{"ubuntu": "http://example.com"}
	err := os.WriteFile(filepath.Join(config.MetaDirectory, "Release"+fileSuffix), []byte("SHA256:\n"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := NewCacher(config); err != nil {
		t.Fatal(err)
	}
}

// TestCacherRestartWithStaleMeta tests that the lists in stale meta data
// files cached do not override those in the newer ones at startup.
func TestCacherRestartWithStaleMeta(t *testing.T) {
	t.Parallel()

	const (
		dists       = "ubuntu/dists/noble/"
		index       = "main/i18n/Index"
		translation = "main/i18n/Translation-en.bz2"
		packages    = "main/binary-amd64/Packages"
	)
	sha1Hex := func(data []byte) string {
		fi, err := makeFileInfo("a", data)
		if err != nil {
			t.Fatal(err)
		}
		return path.Base(fi.SHA1Path())
	}
	makeIndex := func(tr []byte) []byte {
		return fmt.Appendf(nil, "SHA1:\n %s %d Translation-en.bz2\n", sha1Hex(tr), len(tr))
	}
	makePackages := func(name string) []byte {
		data := "deb data of " + name
		return fmt.Appendf(nil, "Package: %s\nFilename: pool/%s.deb\nSize: %d\nSHA256: %s\n",
			name, name, len(data), sha256Hex([]byte(data)))
	}
	makeRelease := func(date string, files map[string][]byte) []byte {
		b := fmt.Appendf(nil, "Date: %s\nSHA256:\n", date)
		for _, name := range slices.Sorted(maps.Keys(files)) {
			b = fmt.Appendf(b, " %s %d %s\n", sha256Hex(files[name]), len(files[name]), name)
		}
		return b
	}
	oldTranslation := []byte("old translation")
	newTranslation := []byte("new translation")
	oldPackages := makePackages("old")
	newPackages := makePackages("new")

	testCases := []struct {
		name string
		// files are the meta data files cached.
		files map[string][]byte
		// want are the checksums that the cacher should know.
		want map[string][]byte
		// listed and unlisted are items listed in the meta data files
		// that the cacher should, or should not know.
		listed, unlisted []string
	}{
		{
			name: "Index not downloaded again",
			files: map[string][]byte{
				"Release": makeRelease("Sat, 18 Oct 2026 00:00:00 UTC", map[string][]byte{
					index:       makeIndex(newTranslation),
					translation: newTranslation,
				}),
				index: makeIndex(oldTranslation),
			},
			want: map[string][]byte{translation: newTranslation},
		},
		{
			name: "Release older than InRelease",
			files: map[string][]byte{
				"InRelease": makeRelease("Sat, 18 Oct 2026 00:00:00 UTC", map[string][]byte{packages: newPackages}),
				"Release":   makeRelease("Fri, 17 Oct 2026 00:00:00 UTC", map[string][]byte{packages: oldPackages}),
				packages:    newPackages,
			},
			want:     map[string][]byte{packages: newPackages},
			listed:   []string{"ubuntu/pool/new.deb"},
			unlisted: []string{"ubuntu/pool/old.deb"},
		},
		{
			name: "InRelease older than Release",
			files: map[string][]byte{
				"InRelease": makeRelease("Fri, 17 Oct 2026 00:00:00 UTC", map[string][]byte{packages: oldPackages}),
				"Release":   makeRelease("Sat, 18 Oct 2026 00:00:00 UTC", map[string][]byte{packages: newPackages}),
				packages:    newPackages,
			},
			want:     map[string][]byte{packages: newPackages},
			listed:   []string{"ubuntu/pool/new.deb"},
			unlisted: []string{"ubuntu/pool/old.deb"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			config := NewConfig()
			config.MetaDirectory = t.TempDir()
			config.CacheDirectory = t.TempDir()
			config.Mapping = map[string]string{"ubuntu": "http://example.com"}
			for name, data := range tc.files {
				fp := filepath.Join(config.MetaDirectory, dists+name+fileSuffix)
				if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fp, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			c, err := NewCacher(config)
			if err != nil {
				t.Fatal(err)
			}
			for name, data := range tc.want {
				want, err := makeFileInfo(dists+name, data)
				if err != nil {
					t.Fatal(err)
				}
				if fi := c.info[dists+name]; fi == nil || !fi.Same(want) {
					t.Errorf("info[%s] = %v, want %v", name, fi, want)
				}
			}
			for _, p := range tc.listed {
				if _, ok := c.info[p]; !ok {
					t.Errorf("%s is not listed", p)
				}
			}
			for _, p := range tc.unlisted {
				if _, ok := c.info[p]; ok {
					t.Errorf("%s is listed", p)
				}
			}
		})
	}
}

func TestCacherGetLocalFailureStopsDownload(t *testing.T) {
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

	c := newTestCacher(t, srv.URL)

	// removing the cache directory makes TempFile fail.
	if err := os.RemoveAll(c.items.dir); err != nil {
		t.Fatal(err)
	}
	if status, _ := getData(t, c, "ubuntu/pool/a.deb"); status != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if n := sent.Load(); n > total/4 {
		t.Errorf("upstream sent %d MiB of %d MiB after a local failure", n>>20, total>>20)
	}
}

func TestCacherGetReleaseUpdatedWhileDownloading(t *testing.T) {
	t.Parallel()

	const (
		release  = "dists/noble/Release"
		packages = "dists/noble/main/binary-amd64/Packages"
	)
	makePackages := func(name string) []byte {
		data := "deb data of " + name
		return fmt.Appendf(nil, "Package: %s\nFilename: pool/%s.deb\nSize: %d\nSHA256: %s\n",
			name, name, len(data), sha256Hex([]byte(data)))
	}
	makeRelease := func(pkgs []byte) []byte {
		return fmt.Appendf(nil, "SHA256:\n %s %d main/binary-amd64/Packages\n",
			sha256Hex(pkgs), len(pkgs))
	}
	oldPackages := makePackages("old")
	newPackages := makePackages("new")

	testCases := []struct {
		name string
		// served is the data served for the download of Packages that
		// started before Release is updated.
		served []byte
	}{
		{
			name:   "matching the updated Release",
			served: newPackages,
		},
		{
			name:   "matching neither",
			served: []byte("broken"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tr := newTestRepo()
			tr.set(release, makeRelease(oldPackages))
			tr.set(packages, oldPackages)

			started := make(chan struct{})
			unblock := make(chan struct{})
			var blocked atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/"+packages && blocked.CompareAndSwap(false, true) {
					close(started)
					<-unblock
					_, _ = w.Write(tc.served)
					return
				}
				tr.ServeHTTP(w, r)
			}))
			defer srv.Close()

			c := newTestCacher(t, srv.URL)
			if status, _ := getData(t, c, "ubuntu/"+release); status != http.StatusOK {
				t.Fatalf("status of Release = %d", status)
			}

			type result struct {
				status int
				data   string
			}
			done := make(chan result, 1)
			go func() {
				status, data := getData(t, c, "ubuntu/"+packages)
				done <- result{status, data}
			}()

			<-started
			tr.set(release, makeRelease(newPackages))
			tr.set(packages, newPackages)
			<-c.Download("ubuntu/"+release, nil)
			close(unblock)

			select {
			case r := <-done:
				if r.status != http.StatusOK || r.data != string(newPackages) {
					t.Errorf("Get = %d, %q, want the updated Packages", r.status, r.data)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Get did not return")
			}
		})
	}
}
