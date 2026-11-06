//go:build windows

package cron

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// Locks the first byte without waiting like msvcrt LK_NBLCK, contention reported as EACCES
func tryFlockOnce(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return syscall.EACCES
	}
	return err
}
