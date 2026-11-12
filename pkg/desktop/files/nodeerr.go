package files

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

var uvDescriptions = map[string]string{
	"E2BIG": "argument list too long", "EACCES": "permission denied", "EAGAIN": "resource temporarily unavailable",
	"EBADF": "bad file descriptor", "EBUSY": "resource busy or locked", "ECANCELED": "operation canceled",
	"EEXIST": "file already exists", "EFAULT": "bad address in system call argument", "EFBIG": "file too large",
	"EINTR": "interrupted system call", "EINVAL": "invalid argument", "EIO": "i/o error",
	"EISDIR": "illegal operation on a directory", "ELOOP": "too many symbolic links encountered",
	"EMFILE": "too many open files", "EMLINK": "too many links", "ENAMETOOLONG": "name too long",
	"ENFILE": "file table overflow", "ENODEV": "no such device", "ENOENT": "no such file or directory",
	"ENOMEM": "not enough memory", "ENOSPC": "no space left on device", "ENOSYS": "function not implemented",
	"ENOTDIR": "not a directory", "ENOTEMPTY": "directory not empty", "ENOTSUP": "operation not supported on socket",
	"ENXIO": "no such device or address", "EOVERFLOW": "value too large for defined data type",
	"EPERM": "operation not permitted", "EPIPE": "broken pipe", "EROFS": "read-only file system",
	"ESPIPE": "invalid seek", "ETIMEDOUT": "connection timed out", "ETXTBSY": "text file is busy",
	"EXDEV": "cross-device link not permitted", "UNKNOWN": "unknown error",
}

// NodeError carries the code and text Node reports for the same failed call
type NodeError struct {
	Code    string
	Message string
	Err     error
}

func (e *NodeError) Error() string { return e.Message }
func (e *NodeError) Unwrap() error { return e.Err }

// Code of a failure the way an IPC handler reads error.code, empty when it carries none
func ErrorCode(err error) string {
	var ne *NodeError
	if errors.As(err, &ne) {
		return ne.Code
	}
	var ie *IpcPathError
	if errors.As(err, &ie) {
		return ie.Code
	}
	var errno syscall.Errno
	hasErrno := errors.As(err, &errno)
	if hasErrno {
		if code, ok := uvCode(errno); ok {
			return code
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, fs.ErrExist):
		return "EEXIST"
	case errors.Is(err, fs.ErrPermission):
		return "EACCES"
	case hasErrno:
		return "UNKNOWN"
	}
	return ""
}

// Node style error for a failed file system call, code then description, syscall and quoted paths
func nodeFsError(err error, syscallName, path string, dest ...string) error {
	if err == nil {
		return nil
	}
	code := ErrorCode(err)
	if code == "" {
		code = "UNKNOWN"
	}
	desc := uvDescriptions[code]
	if desc == "" {
		desc = uvDescriptions["UNKNOWN"]
	}
	msg := fmt.Sprintf("%s: %s, %s '%s'", code, desc, syscallName, path)
	if len(dest) > 0 {
		msg += fmt.Sprintf(" -> '%s'", dest[0])
	}
	return &NodeError{Code: code, Message: msg, Err: err}
}

// True for a missing path or a missing parent, the ENOENT and ENOTDIR pair
func isMissingErr(err error) bool {
	code := ErrorCode(err)
	return code == "ENOENT" || code == "ENOTDIR"
}
