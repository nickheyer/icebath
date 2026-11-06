//go:build freebsd || openbsd || netbsd

package proc

import (
	"errors"
	"os"
	"syscall"
)

// Process error psutil raises for a failed kinfo sysctl of pid
func kinfoError(pid int, name string, err error) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err
	}
	switch errno {
	case syscall.ESRCH:
		return ErrNoSuchProcess
	case syscall.EPERM, syscall.EACCES:
		return ErrAccessDenied
	}
	return os.NewSyscallError(name, errno)
}
