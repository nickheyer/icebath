//go:build openbsd

package pyfs

import (
	"os"

	"golang.org/x/sys/unix"
)

// Clears the file flags and sets mode 0700 on path, leaving a symbolic link untouched
func resetPerms(path string) error {
	if isSymlink(path) {
		return nil
	}
	if err := unix.Chflags(path, 0); err != nil {
		return &os.PathError{Op: "chflags", Path: path, Err: err}
	}
	return os.Chmod(path, 0o700)
}
