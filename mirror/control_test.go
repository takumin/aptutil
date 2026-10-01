package mirror

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGCBrokenSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mustMkdir := func(name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustSymlink := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	mustMkdir(".ubuntu.20260101_000000/ubuntu")
	mustSymlink(filepath.Join(dir, ".ubuntu.20260101_000000/ubuntu"), "ubuntu")
	mustMkdir(".ubuntu.20250101_000000/ubuntu")
	mustSymlink(filepath.Join(dir, ".gone.20250101_000000/gone"), "gone")

	if err := gc(context.Background(), &Config{Dir: dir}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"ubuntu", ".ubuntu.20260101_000000", "gone"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should be kept: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, ".ubuntu.20250101_000000")); !os.IsNotExist(err) {
		t.Errorf("old mirror should be removed: %v", err)
	}
}

func TestGCKeepsOtherFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{".ubuntu.20250101_000000/ubuntu", "internal/pool", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"index.html", ".lock", ".ubuntu.20250101_000001"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil { //nolint:gosec // G306: test files
			t.Fatal(err)
		}
	}

	if err := gc(context.Background(), &Config{Dir: dir}); err != nil {
		t.Fatal(err)
	}

	// files not created by go-apt-mirror are kept, even a regular file
	// whose name looks like a mirror directory.
	for _, name := range []string{"internal/pool", ".hidden", "index.html", ".lock", ".ubuntu.20250101_000001"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should be kept: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, ".ubuntu.20250101_000000")); !os.IsNotExist(err) {
		t.Errorf("old mirror should be removed: %v", err)
	}
}

func TestUpdateMirrorsIsolatesFailures(t *testing.T) {
	t.Parallel()

	repo := flatRepo(map[string]string{"a.deb": "a"})
	repo["a.deb"] = "a"
	good := serveRepo(t, repo)
	// no Release is found.
	bad := serveRepo(t, map[string]string{})

	mirrConfig := func(srv *httptest.Server) *MirrConfig {
		u, err := url.Parse(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		return &MirrConfig{URL: tomlURL{u}, Suites: []string{"/"}}
	}
	c := NewConfig()
	c.Dir = t.TempDir()
	c.Mirrors = map[string]*MirrConfig{
		"good": mirrConfig(good),
		"bad":  mirrConfig(bad),
		// an invalid configuration fails before starting updates.
		"invalid": {URL: tomlURL{&url.URL{Scheme: "http", Host: "localhost"}}},
	}

	err := updateMirrors(context.Background(), c, []string{"good", "bad", "invalid"})
	if err == nil {
		t.Fatal("updateMirrors must fail")
	}
	if !strings.Contains(err.Error(), "bad, invalid") {
		t.Errorf("err = %v, want failed mirrors listed", err)
	}

	data, err := os.ReadFile(filepath.Join(c.Dir, "good", "a.deb"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a" {
		t.Errorf("a.deb = %q, want %q", data, "a")
	}
	if _, err := os.Lstat(filepath.Join(c.Dir, "bad")); !os.IsNotExist(err) {
		t.Errorf("bad must not be published: %v", err)
	}
}
