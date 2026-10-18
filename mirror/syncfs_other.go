//go:build !linux

package mirror

import (
	"io/fs"
	"os"
	"path/filepath"
)

// fsSyncer commits all files and directories in a directory to the
// storage by calling fsync(2) on each of them.
type fsSyncer struct {
	dir string
}

// newFSSyncer returns fsSyncer to sync d later.
func newFSSyncer(d string) (*fsSyncer, error) {
	return &fsSyncer{dir: d}, nil
}

// Sync commits all files and directories in the directory.
func (s *fsSyncer) Sync() error {
	return filepath.WalkDir(s.dir, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !de.IsDir() && !de.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p) //nolint:gosec // G304: p is in a mirror directory, opened read-only to sync it
		if err != nil {
			return err
		}
		err = f.Sync()
		if err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	})
}

// Close does nothing.
func (s *fsSyncer) Close() error {
	return nil
}
