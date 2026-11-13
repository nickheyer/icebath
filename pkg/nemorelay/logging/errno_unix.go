//go:build !windows

package logging

import "syscall"

// Reason name of an errno the sink open failure distinguishes
func errnoReason(errno syscall.Errno) string {
	switch errno {
	case syscall.ENOTDIR:
		return "not_a_directory"
	case syscall.EISDIR:
		return "is_a_directory"
	case syscall.EROFS:
		return "read_only_filesystem"
	case syscall.ENOSPC:
		return "storage_full"
	}
	return ""
}
