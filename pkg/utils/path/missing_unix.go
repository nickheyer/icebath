//go:build !windows

package pathutil

import "syscall"

// Errnos besides the not-found ones that mean a path is not there
func missingErrno(errno syscall.Errno) bool {
	return errno == syscall.ENOTDIR || errno == syscall.ELOOP
}
