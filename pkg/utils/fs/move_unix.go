//go:build !windows

package fsutil

import (
	"os"
	"syscall"
)

// The bare rename call, which unlike os.Rename lets a directory replace an empty directory as POSIX allows
func moveRename(src, dst string) error {
	if err := syscall.Rename(src, dst); err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}
