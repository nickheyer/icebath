package pypath

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
)

// Whether a Go error stands for a Python OSError or one of its subclasses
func IsOSError(err error) bool {
	var pe *fs.PathError
	var le *os.LinkError
	var se *os.SyscallError
	var errno syscall.Errno
	var ee *exec.Error
	if errors.As(err, &pe) || errors.As(err, &le) || errors.As(err, &se) || errors.As(err, &errno) || errors.As(err, &ee) {
		return true
	}
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrPermission)
}
