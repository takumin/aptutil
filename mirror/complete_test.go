package mirror

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// updateOnce mirrors files served at suites once, and returns the
// directory of the mirror.
func updateOnce(t *testing.T, files map[string]string, suites []string, allowUnsigned bool) (string, error) {
	t.Helper()

	srv := serveRepo(t, files)
	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c := NewConfig()
	c.Dir = t.TempDir()
	c.Mirrors = map[string]*MirrConfig{
		"test": {URL: tomlURL{u}, Suites: suites, AllowUnsigned: allowUnsigned},
	}
	m, err := NewMirror(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "test", c)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(c.Dir, "test"), m.Update(context.Background())
}

// releaseOf returns a Release file listing the given indices.
func releaseOf(indices map[string]string) string {
	var b strings.Builder
	b.WriteString("SHA256:\n")
	for name, data := range indices {
		sum := sha256.Sum256([]byte(data))
		fmt.Fprintf(&b, " %s %d %s\n", hex.EncodeToString(sum[:]), len(data), name)
	}
	return b.String()
}

func gzipped(t *testing.T, s string) string {
	t.Helper()

	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestMirrorRequiresSignature(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		remove        []string
		inRelease     bool
		allowUnsigned bool
		wantErr       bool
	}{
		{name: "Release.gpg"},
		{name: "InRelease", remove: []string{"Release", "Release.gpg"}, inRelease: true},
		{name: "unsigned", remove: []string{"Release.gpg"}, wantErr: true},
		{name: "Release.gpg without Release", remove: []string{"Release"}, wantErr: true},
		{name: "unsigned allowed", remove: []string{"Release.gpg"}, allowUnsigned: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			files := flatRepo(map[string]string{"a.deb": "a"})
			files["a.deb"] = "a"
			if tc.inRelease {
				files["InRelease"] = files["Release"]
			}
			for _, p := range tc.remove {
				delete(files, p)
			}

			dir, err := updateOnce(t, files, []string{"/"}, tc.allowUnsigned)
			if tc.wantErr {
				if err == nil {
					t.Error("update must fail")
				}
				if _, err := os.Lstat(dir); !os.IsNotExist(err) {
					t.Errorf("mirror must not be published: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "a.deb")); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestMirrorInconsistentReleases(t *testing.T) {
	t.Parallel()

	for _, byhash := range []bool{false, true} {
		t.Run(fmt.Sprintf("byhash=%v", byhash), func(t *testing.T) {
			t.Parallel()

			files := flatRepo(map[string]string{"a.deb": "a"})
			files["a.deb"] = "a"
			// InRelease of the next generation listing b.deb.
			next := flatRepo(map[string]string{"a.deb": "a", "b.deb": "b"})
			if byhash {
				files["Release"] = "Acquire-By-Hash: yes\n" + files["Release"]
				next["Release"] = "Acquire-By-Hash: yes\n" + next["Release"]
			}
			files["InRelease"] = next["Release"]

			dir, err := updateOnce(t, files, []string{"/"}, false)
			if err == nil {
				t.Error("update must fail")
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Errorf("mirror must not be published: %v", err)
			}
		})
	}
}

func TestMirrorConsistentReleases(t *testing.T) {
	t.Parallel()

	files := flatRepo(map[string]string{"a.deb": "a"})
	files["a.deb"] = "a"
	files["InRelease"] = "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" +
		files["Release"] +
		"-----BEGIN PGP SIGNATURE-----\n\ndummy\n-----END PGP SIGNATURE-----\n"

	dir, err := updateOnce(t, files, []string{"/"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Release", "Release.gpg", "InRelease", "a.deb"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Error(err)
		}
	}
}

func TestMirrorMissingIndex(t *testing.T) {
	t.Parallel()

	packages := "Package: a\nFilename: a.deb\nSize: 1\n"
	packagesGz := gzipped(t, packages)

	testCases := []struct {
		name    string
		listed  map[string]string
		served  []string
		wantErr bool
	}{
		{
			name:   "all formats",
			listed: map[string]string{"Packages": packages, "Packages.gz": packagesGz},
			served: []string{"Packages", "Packages.gz"},
		},
		{
			name:   "only gz",
			listed: map[string]string{"Packages": packages, "Packages.gz": packagesGz},
			served: []string{"Packages.gz"},
		},
		{
			name:    "none",
			listed:  map[string]string{"Packages": packages, "Packages.gz": packagesGz},
			wantErr: true,
		},
		{
			name:    "only unsupported format",
			listed:  map[string]string{"Packages.zst": "zstd"},
			served:  []string{"Packages.zst"},
			wantErr: true,
		},
		{
			// indices not scanned for items may be missing.
			name:   "not scanned",
			listed: map[string]string{"Packages": packages, "Contents-amd64": "contents"},
			served: []string{"Packages"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			files := map[string]string{
				"Release":     releaseOf(tc.listed),
				"Release.gpg": "dummy",
				"a.deb":       "a",
			}
			for _, p := range tc.served {
				files[p] = tc.listed[p]
			}

			dir, err := updateOnce(t, files, []string{"/"}, false)
			if tc.wantErr {
				if err == nil {
					t.Error("update must fail")
				}
				if _, err := os.Lstat(dir); !os.IsNotExist(err) {
					t.Errorf("mirror must not be published: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "a.deb")); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestMirrorConflictingItems(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		a, b    string
		wantErr bool
	}{
		{name: "same", a: "x", b: "x"},
		{name: "different", a: "x", b: "y", wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// flat suites a/ and b/ list x.deb at the same path.
			// serve the one that the last suite lists.
			files := map[string]string{"x.deb": tc.b}
			for suite, data := range map[string]string{"a/": tc.a, "b/": tc.b} {
				for p, s := range flatRepo(map[string]string{"x.deb": data}) {
					files[suite+p] = s
				}
			}

			dir, err := updateOnce(t, files, []string{"a/", "b/"}, false)
			if tc.wantErr {
				if err == nil {
					t.Error("update must fail")
				}
				if _, err := os.Lstat(dir); !os.IsNotExist(err) {
					t.Errorf("mirror must not be published: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "x.deb")); err != nil {
				t.Error(err)
			}
		})
	}
}
