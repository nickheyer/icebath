//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package fsutil

import (
	"io/fs"
	"syscall"
)

// User and system immutable file flags
const immutableFlags = 0x2 | 0x20000

// Whether the user or the system immutable flag is set on path
func isImmutable(path string) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	return st.Flags&immutableFlags != 0, nil
}
