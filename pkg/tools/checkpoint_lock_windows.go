//go:build windows

package tools

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Locks the first byte without waiting, false when another holder has it
func checkpointTryLock(f *os.File) (bool, error) {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return false, nil
	}
	return false, err
}
