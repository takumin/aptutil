package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cybozu-go/aptutil/apt"
)

// flatRepo returns the files of a flat repository whose Packages lists
// the given items with their contents.  Release.gpg is a dummy as
// signatures are not verified.
func flatRepo(items map[string]string) map[string]string {
	sha256hex := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}

	var pkgs strings.Builder
	for name, data := range items {
		fmt.Fprintf(&pkgs, "Package: %s\nFilename: %s\nSize: %d\nSHA256: %s\n\n",
			name, name, len(data), sha256hex(data))
	}
	packages := pkgs.String()
	release := fmt.Sprintf("SHA256:\n %s %d Packages\n", sha256hex(packages), len(packages))

	return map[string]string{
		"Release":     release,
		"Release.gpg": "dummy",
		"Packages":    packages,
	}
}

// serveRepo serves files.  Files not in files are not found.
func serveRepo(t *testing.T, files map[string]string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(data))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMirrorPoolUpdateFailureKeepsOldMirror(t *testing.T) {
	t.Parallel()

	files := flatRepo(map[string]string{"a.deb": "a"})
	files["a.deb"] = "a"
	var repo atomic.Pointer[map[string]string]
	repo.Store(&files)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := (*repo.Load())[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(data))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c := NewConfig()
	c.Dir = t.TempDir()
	c.Mirrors = map[string]*MirrConfig{
		"test": {URL: tomlURL{u}, Suites: []string{"/"}},
	}
	update := func(tm time.Time) error {
		m, err := NewMirror(tm, "test", c)
		if err != nil {
			t.Fatal(err)
		}
		return m.Update(context.Background())
	}

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := update(t1); err != nil {
		t.Fatal(err)
	}

	// the upstream lists b.deb but does not serve it.
	files2 := flatRepo(map[string]string{"a.deb": "a", "b.deb": "b"})
	files2["a.deb"] = "a"
	repo.Store(&files2)
	if err := update(t1.Add(time.Hour)); err == nil {
		t.Fatal("update must fail when a pool file fails to download")
	}
	if err := gc(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	// the old mirror is still published as a whole.
	mustRead := func(p string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(c.Dir, "test", p)) //nolint:gosec // G304: p is a fixed file name in the test
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got := mustRead("Packages"); got != files["Packages"] {
		t.Errorf("Packages = %q, want the old one", got)
	}
	if got := mustRead("a.deb"); got != "a" {
		t.Errorf("a.deb = %q, want %q", got, "a")
	}
	dirs, err := filepath.Glob(filepath.Join(c.Dir, ".test.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 {
		t.Errorf("mirror directories = %v, want only the old one", dirs)
	}
}

func TestMirrorUpdateRedownloadsMissingFile(t *testing.T) {
	t.Parallel()

	files := flatRepo(map[string]string{"a.deb": "a"})
	files["a.deb"] = "a"
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "a.deb" {
			hits.Add(1)
		}
		data, ok := files[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(data))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c := NewConfig()
	c.Dir = t.TempDir()
	c.Mirrors = map[string]*MirrConfig{
		"test": {URL: tomlURL{u}, Suites: []string{"/"}},
	}
	update := func(tm time.Time) error {
		m, err := NewMirror(tm, "test", c)
		if err != nil {
			t.Fatal(err)
		}
		return m.Update(context.Background())
	}

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := update(t1); err != nil {
		t.Fatal(err)
	}

	// a file of the current mirror is removed by hand.
	if err := os.Remove(filepath.Join(c.Dir, "test", "a.deb")); err != nil {
		t.Fatal(err)
	}
	if err := update(t1.Add(time.Hour)); err != nil {
		t.Fatalf("update must download the missing file again: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(c.Dir, "test", "a.deb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a" {
		t.Errorf("a.deb = %q, want %q", data, "a")
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("a.deb was downloaded %d times, want 2", n)
	}
}

// TestReuseOrDownloadCanceled is not run in parallel to count goroutines
// left by reuseOrDownload.
func TestReuseOrDownloadCanceled(t *testing.T) {
	m := newTestMirror(t, "http://example.com")
	fil := []*apt.FileInfo{apt.MakeFileInfoNoChecksum("a", 1)}

	// workers may not be started for the canceled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 1000 {
		results := make(chan *dlResult, 1)
		if _, err := m.reuseOrDownload(ctx, fil, false, results); err == nil {
			t.Fatal("reuseOrDownload must fail for the canceled context")
		}
	}

	left := func() int {
		buf := make([]byte, 1<<22)
		n := 0
		for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			if strings.Contains(g, "reuseOrDownload") {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(5 * time.Second)
	for left() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines of reuseOrDownload are left", left())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
