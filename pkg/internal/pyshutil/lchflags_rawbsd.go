//go:build freebsd || netbsd || dragonfly

package pyshutil

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// Sets the file flags of a symlink itself with the lchflags system call
func lchflags(path string, flags uint32) error {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	if _, _, errno := unix.Syscall(unix.SYS_LCHFLAGS, uintptr(unsafe.Pointer(p)), uintptr(flags), 0); errno != 0 {
		return errno
	}
	return nil
}
