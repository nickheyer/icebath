package pathutil

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows errors besides the not-found ones that mean a path is not there
func missingErrno(errno syscall.Errno) bool {
	switch errno {
	case windows.ERROR_INVALID_DRIVE, windows.ERROR_NOT_READY, windows.ERROR_BAD_NET_NAME, windows.ERROR_INVALID_NAME,
		windows.ERROR_BAD_PATHNAME, windows.ERROR_FILENAME_EXCED_RANGE, windows.ERROR_DIRECTORY, windows.ERROR_CANT_RESOLVE_FILENAME:
		return true
	}
	return false
}
