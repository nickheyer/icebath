package pyshutil

import (
	"os"
	"syscall"
)

// Copies the read only attribute of one symlink onto another, all shutil.copystat changes on a Windows link
func CopyLinkStat(src, dst string) error {
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
		return &os.PathError{Op: "stat", Path: src, Err: err}
	}
	dstAttrs, err := syscall.GetFileAttributes(to)
	if err != nil {
		return &os.PathError{Op: "chmod", Path: dst, Err: err}
	}
	want := dstAttrs &^ syscall.FILE_ATTRIBUTE_READONLY
	if srcAttrs&syscall.FILE_ATTRIBUTE_READONLY != 0 {
		want |= syscall.FILE_ATTRIBUTE_READONLY
	}
	if want == dstAttrs {
		return nil
	}
	if err := syscall.SetFileAttributes(to, want); err != nil {
		return &os.PathError{Op: "chmod", Path: dst, Err: err}
	}
	return nil
}
