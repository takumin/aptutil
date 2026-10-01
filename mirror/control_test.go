package mirror

import (
	"context"
	"os"
	"path/filepath"
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
