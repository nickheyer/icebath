//go:build windows

package pyfs

import (
	"os"

	"golang.org/x/sys/windows"
)

// Clears the read only attribute of path itself, a link included, like os.lchmod with mode 0700
func resetPerms(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return &os.PathError{Op: "chmod", Path: path, Err: err}
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return &os.PathError{Op: "chmod", Path: path, Err: err}
	}
	if err := windows.SetFileAttributes(p, attrs&^windows.FILE_ATTRIBUTE_READONLY); err != nil {
		return &os.PathError{Op: "chmod", Path: path, Err: err}
	}
	return nil
}
