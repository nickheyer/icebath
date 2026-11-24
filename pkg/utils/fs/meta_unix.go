//go:build !windows

package fsutil

import (
	"io/fs"
	"os"
	"syscall"
)

// All twelve permission bits of the file path names, symlinks followed, false when it cannot be read
func PermBits(path string) (uint32, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, false
	}
	return uint32(st.Mode) & 0o7777, true
}

// User and group ids owning the file path names, symlinks followed, nil when it cannot be read
func OwnerOf(path string) *Owner {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return nil
	}
	return &Owner{UID: int(st.Uid), GID: int(st.Gid)}
}

func chown(path string, uid, gid int) { _ = os.Chown(path, uid, gid) }

func chmodBits(path string, mode uint32) error {
	if err := syscall.Chmod(path, mode&0o7777); err != nil {
		return &fs.PathError{Op: "chmod", Path: path, Err: err}
	}
	return nil
}
