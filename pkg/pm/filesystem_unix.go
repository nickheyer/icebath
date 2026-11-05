//go:build !windows

package pm

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Tries the exclusive flock once, false while another description holds it
func tryLockFile(f *os.File) (bool, error) {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return false, nil
		}
		return false, err
	}
}

// Drops the flock held on the file
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// POSIX carries no reparse tags so nothing is a mount point junction
func reparseIsMountPoint(string) bool { return false }

// Errno values a transient hold on a file surfaces as
func isHeldError(err error) bool {
	return isAccessError(err) || errors.Is(err, syscall.EBUSY)
}

// Errno values Python raises as PermissionError
func isAccessError(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}

// Removes a non directory entry the way os.unlink does, refusing a directory instead of removing it
func unlinkPath(path string) error {
	for {
		err := syscall.Unlink(path)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EINTR) {
			return &fs.PathError{Op: "unlink", Path: path, Err: err}
		}
	}
}
