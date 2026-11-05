//go:build windows

package pypath

import "syscall"

var errnoClasses = map[syscall.Errno]string{
	syscall.ERROR_FILE_NOT_FOUND: "FileNotFoundError", syscall.ERROR_PATH_NOT_FOUND: "FileNotFoundError",
	syscall.ERROR_ACCESS_DENIED: "PermissionError", syscall.ERROR_ALREADY_EXISTS: "FileExistsError",
	syscall.ERROR_FILE_EXISTS: "FileExistsError", syscall.ERROR_BROKEN_PIPE: "BrokenPipeError",
	syscall.Errno(267): "NotADirectoryError", syscall.Errno(10053): "ConnectionAbortedError",
	syscall.Errno(10061): "ConnectionRefusedError", syscall.WSAECONNRESET: "ConnectionResetError",
	syscall.Errno(10035): "BlockingIOError", syscall.Errno(232): "BrokenPipeError",
}
