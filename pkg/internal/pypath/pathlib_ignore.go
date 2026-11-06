package pypath

import (
	"errors"
	"runtime"
	"syscall"
)

// True for the errors pathlib exists and is_dir turn into False, ENOENT ENOTDIR EBADF ELOOP and three winerrors
func PathlibIgnoredError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	if runtime.GOOS == "windows" && (errno == 21 || errno == 123 || errno == 1921) {
		return true
	}
	switch name, _ := ErrnoName(err); name {
	case "ENOENT", "ENOTDIR", "EBADF", "ELOOP":
		return true
	}
	return false
}
