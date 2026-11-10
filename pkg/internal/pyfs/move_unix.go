//go:build !windows

package pyfs

import (
	"os"
	"syscall"
)

// os.rename, the bare rename call so an empty directory target is replaced as POSIX allows
func osRename(src, dst string) error {
	if err := syscall.Rename(src, dst); err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}
