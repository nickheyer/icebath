//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package pyfs

import (
	"os"
	"syscall"
)

// Whether st_flags of the path is exactly the user or the system immutable flag
func isImmutable(path string) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return false, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return st.Flags == 0x2 || st.Flags == 0x20000, nil
}
