//go:build !windows

package pyfs

import (
	"errors"
	"os"
	"syscall"
)

// Mode a plain create gives a new file under the process umask
func defaultNewFileMode() (os.FileMode, bool) {
	current := syscall.Umask(0o077)
	syscall.Umask(current)
	return os.FileMode(0o666 &^ current), true
}

// Text mode writes keep LF on POSIX hosts
func PlatformNewlines(s string) string { return s }

// Contended rename retries only exist on Windows
func IsContendedReplaceError(err error) bool { return false }

// Cross device and busy target renames fall back to a copy
func IsCrossDeviceError(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EBUSY)
}

// Sets all twelve permission bits on the open staged file through fchmod before it is published
func fchmodStaged(f *os.File, mode uint32) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var chmodErr error
	if err := conn.Control(func(fd uintptr) { chmodErr = syscall.Fchmod(int(fd), mode&0o7777) }); err != nil {
		return err
	}
	if chmodErr != nil {
		return &os.PathError{Op: "chmod", Path: f.Name(), Err: chmodErr}
	}
	return nil
}
