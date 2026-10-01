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

func TestGCRemovesStaleTmpLink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{".ubuntu.20260101_000000/ubuntu", ".ubuntu.20260102_000000/ubuntu", "other"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"ubuntu":     ".ubuntu.20260101_000000/ubuntu",
		"ubuntu.tmp": ".ubuntu.20260102_000000/ubuntu",
		"other.tmp":  "other",
	}
	for name, target := range links {
		if err := os.Symlink(filepath.Join(dir, target), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}

	if err := gc(context.Background(), &Config{Dir: dir}); err != nil {
		t.Fatal(err)
	}

	// the stale temporary symlink and the mirror it pointed to are removed.
	for _, name := range []string{"ubuntu.tmp", ".ubuntu.20260102_000000"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed: %v", name, err)
		}
	}
	// a symlink that happens to end with ".tmp" is kept.
	for _, name := range []string{"ubuntu", ".ubuntu.20260101_000000", "other.tmp", "other"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should be kept: %v", name, err)
		}
	}
}

func TestIsMirrorTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		target, id string
		want       bool
	}{
		{"/var/spool/go-apt-mirror/.ubuntu.20260101_000000/ubuntu", "ubuntu", true},
		{".ubuntu.20260101_000000/ubuntu", "ubuntu", true},
		{"/var/spool/go-apt-mirror/.ubuntu.20260101_000000/ubuntu", "security", false},
		{"/var/spool/go-apt-mirror/.ubuntu-old.20260101_000000/ubuntu", "ubuntu", false},
		{"/var/spool/go-apt-mirror/.ubuntu.20260101/ubuntu", "ubuntu", false},
		{"/var/spool/go-apt-mirror/ubuntu", "ubuntu", false},
		{"/srv/repo", "repo", false},
	}
	for _, tc := range cases {
		if got := isMirrorTarget(tc.target, tc.id); got != tc.want {
			t.Errorf("isMirrorTarget(%q, %q) = %v, want %v", tc.target, tc.id, got, tc.want)
		}
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
