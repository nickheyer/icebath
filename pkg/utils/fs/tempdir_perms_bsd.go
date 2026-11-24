//go:build freebsd || netbsd || dragonfly

package fsutil

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// Clears the file flags and sets mode 0700 on path itself without following a link
func resetPerms(path string) error {
	if err := lchflags(path, 0); err != nil {
		return &fs.PathError{Op: "lchflags", Path: path, Err: err}
	}
	if err := unix.Fchmodat(unix.AT_FDCWD, path, 0o700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "lchmod", Path: path, Err: err}
	}
	return nil
}
