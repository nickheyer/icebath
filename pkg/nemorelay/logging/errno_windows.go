//go:build windows

package logging

import "syscall"

const (
	errorDirectory     syscall.Errno = 267
	errorWriteProtect  syscall.Errno = 19
	errorDiskFull      syscall.Errno = 112
	errorHandleDiskFul syscall.Errno = 39
)

// Reason name of a Windows error the sink open failure distinguishes
func errnoReason(errno syscall.Errno) string {
	switch errno {
	case errorDirectory:
		return "not_a_directory"
	case errorWriteProtect:
		return "read_only_filesystem"
	case errorDiskFull, errorHandleDiskFul:
		return "storage_full"
	}
	return ""
}
