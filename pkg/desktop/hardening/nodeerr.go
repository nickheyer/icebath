package hardening

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

var uvDescriptions = map[string]string{
	"E2BIG": "argument list too long", "EACCES": "permission denied", "EAGAIN": "resource temporarily unavailable",
	"EBADF": "bad file descriptor", "EBUSY": "resource busy or locked", "ECANCELED": "operation canceled",
	"EEXIST": "file already exists", "EFAULT": "bad address in system call argument", "EFBIG": "file too large",
	"EINTR": "interrupted system call", "EINVAL": "invalid argument", "EIO": "i/o error", "EISDIR": "illegal operation on a directory",
	"ELOOP": "too many symbolic links encountered", "EMFILE": "too many open files", "EMLINK": "too many links",
	"ENAMETOOLONG": "name too long", "ENFILE": "file table overflow", "ENODEV": "no such device",
	"ENOENT": "no such file or directory", "ENOMEM": "not enough memory", "ENOSPC": "no space left on device",
	"ENOSYS": "function not implemented", "ENOTDIR": "not a directory", "ENOTEMPTY": "directory not empty",
	"ENOTSUP": "operation not supported on socket", "ENXIO": "no such device or address", "EOVERFLOW": "value too large for defined data type",
	"EPERM": "operation not permitted", "EPIPE": "broken pipe", "EROFS": "read-only file system", "ESPIPE": "invalid seek",
	"ETIMEDOUT": "connection timed out", "ETXTBSY": "text file is busy", "EXDEV": "cross-device link not permitted",
	"UNKNOWN": "unknown error",
}

var nodeSyscalls = map[string]string{
	"open": "open", "read": "read", "write": "write", "close": "close", "stat": "stat", "lstat": "lstat",
	"mkdir": "mkdir", "remove": "unlink", "unlink": "unlink", "rmdir": "rmdir", "rename": "rename",
	"readdirent": "scandir", "readdir": "scandir", "fdopendir": "scandir", "chmod": "chmod", "symlink": "symlink",
	"link": "link", "readlink": "readlink", "copyfile": "copyfile", "chtimes": "utime", "truncate": "ftruncate",
	"sync": "fsync", "seek": "lseek", "realpath": "realpath",
}

var fdSyscalls = map[string]bool{"read": true, "write": true, "close": true, "fsync": true, "ftruncate": true, "lseek": true}

// Error whose text is what Node prints for the same failed file system call
type nodeError struct {
	code string
	msg  string
	err  error
}

func (e *nodeError) Error() string { return e.msg }
func (e *nodeError) Unwrap() error { return e.err }

// Node code name such as ENOENT of an error, UNKNOWN when it carries no errno
func NodeCode(err error) string {
	var ne *nodeError
	if errors.As(err, &ne) {
		return ne.code
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
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
	}
	return "UNKNOWN"
}

// Wraps a file system error so its text matches the Node message for the same call
func NodeErr(err error) error {
	if err == nil {
		return nil
	}
	var ne *nodeError
	if errors.As(err, &ne) {
		return err
	}
	var op, path, dest string
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		op, path = pe.Op, pe.Path
	case errors.As(err, &le):
		op, path, dest = le.Op, le.Old, le.New
	default:
		return err
	}
	code := NodeCode(err)
	syscallName := nodeSyscalls[op]
	if syscallName == "" {
		syscallName = op
	}
	msg := code + ": " + uvDescriptions[code] + ", " + syscallName
	if !fdSyscalls[syscallName] {
		msg += " '" + path + "'"
		if dest != "" {
			msg += " -> '" + dest + "'"
		}
	}
	return &nodeError{code: code, msg: msg, err: err}
}
