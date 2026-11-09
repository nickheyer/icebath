//go:build !windows

package pm

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// Owner uid of a stat result
func fileOwnerUID(info os.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return 0
}

// Reports whether pid answers a null signal, another user's process counting as alive
func stillRunning(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// Reports the unlink refusals hermes reads as PermissionError
func unlinkPermissionDenied(err error) bool { return errors.Is(err, fs.ErrPermission) }
