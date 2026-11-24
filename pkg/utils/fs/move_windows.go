//go:build windows

package fsutil

import (
	"os"

	"golang.org/x/sys/windows"
)

// MoveFileEx without the replace flag, so an existing target fails the rename
func moveRename(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	if err := windows.MoveFileEx(from, to, 0); err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}
