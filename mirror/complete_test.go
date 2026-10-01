package mirror

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
