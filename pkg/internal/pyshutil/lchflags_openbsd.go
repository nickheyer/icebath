package pyshutil

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// Sets the file flags of a symlink itself with chflagsat and the no follow flag
func lchflags(path string, flags uint32) error {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	atFDCWD := unix.AT_FDCWD
	if _, _, errno := unix.Syscall6(unix.SYS_CHFLAGSAT, uintptr(atFDCWD), uintptr(unsafe.Pointer(p)), uintptr(flags), unix.AT_SYMLINK_NOFOLLOW, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
