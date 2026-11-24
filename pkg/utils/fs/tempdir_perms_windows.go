package fsutil

import (
	"io/fs"

	"golang.org/x/sys/windows"
)

// Clears the read only attribute of path itself, a link included
func resetPerms(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return &fs.PathError{Op: "chmod", Path: path, Err: err}
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return &fs.PathError{Op: "chmod", Path: path, Err: err}
	}
	if err := windows.SetFileAttributes(p, attrs&^windows.FILE_ATTRIBUTE_READONLY); err != nil {
		return &fs.PathError{Op: "chmod", Path: path, Err: err}
	}
	return nil
}
