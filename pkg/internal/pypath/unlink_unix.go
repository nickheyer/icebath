//go:build !windows

package pypath

import (
	"io/fs"
	"syscall"
)

// os.unlink, a directory refused with the kernel errno instead of removed
func Unlink(path string) error {
	if err := syscall.Unlink(path); err != nil {
		return &fs.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}
