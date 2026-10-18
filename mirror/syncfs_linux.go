package mirror

import (
	"os"

	"golang.org/x/sys/unix"
)

// fsSyncer commits all changes to the file system containing a
// directory, including files and directories in it, by calling
// syncfs(2).
//
// It is much faster than calling fsync(2) on each of many files.
//
// syncfs(2) reports a write-back error on the file system to each open
// file only once, and does not report an error that had been reported
// to another file before the file was opened.  The directory is
// therefore opened before files are written in it, and kept open until
// they are synced; otherwise, an error reported to another caller of
// syncfs(2), such as another mirror in the same file system, would be
// missed.
type fsSyncer struct {
	f *os.File
}

// newFSSyncer opens d to sync the file system containing d later.
// It must be called before writing files to be synced.
func newFSSyncer(d string) (*fsSyncer, error) {
	f, err := os.Open(d) //nolint:gosec // G304: d is a mirror directory, opened read-only to sync it
	if err != nil {
		return nil, err
	}
	return &fsSyncer{f: f}, nil
}

// Sync commits all changes to the file system.  It returns an error if
// writing back any file in the file system failed since s was opened.
func (s *fsSyncer) Sync() error {
	return os.NewSyscallError("syncfs", unix.Syncfs(int(s.f.Fd()))) //nolint:gosec // G115: file descriptors fit in int
}

// Close closes the directory opened by newFSSyncer.
func (s *fsSyncer) Close() error {
	return s.f.Close()
}
