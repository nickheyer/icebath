//go:build !windows

package tools

import (
	"errors"
	"os"
	"syscall"
)

// Errno a child chdir into dir would fail with, nil when the directory is enterable
func popenChdirError(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			return errno
		}
		return err
	}
	if !st.IsDir() {
		return syscall.ENOTDIR
	}
	return syscall.Access(dir, 0x1)
}
