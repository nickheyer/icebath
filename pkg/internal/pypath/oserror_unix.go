//go:build !windows

package pypath

import "syscall"

var errnoClasses = map[syscall.Errno]string{
	syscall.EAGAIN: "BlockingIOError", syscall.EALREADY: "BlockingIOError", syscall.EINPROGRESS: "BlockingIOError",
	syscall.ECHILD: "ChildProcessError", syscall.EPIPE: "BrokenPipeError", syscall.ESHUTDOWN: "BrokenPipeError",
	syscall.ECONNABORTED: "ConnectionAbortedError", syscall.ECONNREFUSED: "ConnectionRefusedError",
	syscall.ECONNRESET: "ConnectionResetError", syscall.EEXIST: "FileExistsError", syscall.ENOENT: "FileNotFoundError",
	syscall.EINTR: "InterruptedError", syscall.EISDIR: "IsADirectoryError", syscall.ENOTDIR: "NotADirectoryError",
	syscall.EACCES: "PermissionError", syscall.EPERM: "PermissionError", syscall.ESRCH: "ProcessLookupError",
	syscall.ETIMEDOUT: "TimeoutError",
}
