//go:build freebsd || netbsd || dragonfly

package pyfs

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Clears the file flags and sets mode 0700 on path itself without following a link
func resetPerms(path string) error {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return &os.PathError{Op: "lchflags", Path: path, Err: err}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LCHFLAGS, uintptr(unsafe.Pointer(p)), 0, 0); errno != 0 {
		return &os.PathError{Op: "lchflags", Path: path, Err: errno}
	}
	if err := unix.Fchmodat(unix.AT_FDCWD, path, 0o700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "lchmod", Path: path, Err: err}
	}
	return nil
}
