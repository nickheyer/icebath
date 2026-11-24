//go:build windows

package fsutil

import (
	"io/fs"
	"syscall"
)

// Copies the read only attribute of the symlink src onto the symlink dst, the only link metadata Windows lets a copy change
func CopyLinkMetadata(src, dst string) error {
	from, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	srcAttrs, err := syscall.GetFileAttributes(from)
	if err != nil {
		return &fs.PathError{Op: "stat", Path: src, Err: err}
	}
	dstAttrs, err := syscall.GetFileAttributes(to)
	if err != nil {
		return &fs.PathError{Op: "chmod", Path: dst, Err: err}
	}
	want := dstAttrs &^ syscall.FILE_ATTRIBUTE_READONLY
	if srcAttrs&syscall.FILE_ATTRIBUTE_READONLY != 0 {
		want |= syscall.FILE_ATTRIBUTE_READONLY
	}
	if want == dstAttrs {
		return nil
	}
	if err := syscall.SetFileAttributes(to, want); err != nil {
		return &fs.PathError{Op: "chmod", Path: dst, Err: err}
	}
	return nil
}
