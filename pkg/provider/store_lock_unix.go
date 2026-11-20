//go:build !windows

package provider

import (
	"errors"
	"os"
	"syscall"
)

// Released lock files stay empty since flock needs no byte to lock
const lockReleasePad = ""

// kernelLock takes or releases a non blocking exclusive flock on the open lock file
func kernelLock(f *os.File, acquire bool) error {
	how := syscall.LOCK_UN
	if acquire {
		how = syscall.LOCK_EX | syscall.LOCK_NB
	}
	return syscall.Flock(int(f.Fd()), how)
}

// isLockContention reports whether the flock failure means another holder rather than a broken filesystem
func isLockContention(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EDEADLK) || errors.Is(err, syscall.EWOULDBLOCK)
}

// processAlive reports whether a pid still exists on this host
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
}
