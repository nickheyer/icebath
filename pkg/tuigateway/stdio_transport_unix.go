//go:build !windows

package tuigateway

import "syscall"

// True for the errno values a write to a departed peer fails with
func peerGoneErrno(errno syscall.Errno) bool {
	switch errno {
	case syscall.EPIPE, syscall.ECONNRESET, syscall.EBADF, syscall.ESHUTDOWN:
		return true
	}
	return false
}
