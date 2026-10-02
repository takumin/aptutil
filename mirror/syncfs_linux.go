package mirror

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncFS commits all changes to the file system containing d,
// including files and directories in d, by calling syncfs(2).
//
// It is much faster than calling fsync(2) on each of many files.
func syncFS(d string) error {
	f, err := os.Open(d) //nolint:gosec // G304: d is a mirror directory, opened read-only to sync it
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	return os.NewSyscallError("syncfs", unix.Syncfs(int(f.Fd()))) //nolint:gosec // G115: file descriptors fit in int
}
