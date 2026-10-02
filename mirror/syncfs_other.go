//go:build !linux

package mirror

import (
	"io/fs"
	"os"
	"path/filepath"
)

// syncFS commits all files and directories in d to the storage by
// calling fsync(2) on each of them.
func syncFS(d string) error {
	return filepath.WalkDir(d, func(p string, de fs.DirEntry, err error) error {
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
