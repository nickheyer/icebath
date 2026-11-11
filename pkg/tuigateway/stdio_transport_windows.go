//go:build windows

package tuigateway

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// True for the errno values a write to a departed peer fails with, the Winsock reset and shutdown codes included
func peerGoneErrno(errno syscall.Errno) bool {
	switch errno {
	case syscall.EPIPE, syscall.ECONNRESET, syscall.EBADF, syscall.ESHUTDOWN, windows.WSAECONNRESET, windows.WSAESHUTDOWN,
		windows.ERROR_BROKEN_PIPE, windows.ERROR_NO_DATA, windows.ERROR_INVALID_HANDLE:
		return true
	}
	return false
}
