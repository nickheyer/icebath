//go:build windows

package pyfs

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

var lockRetryDelay = time.Second

// Locks the first byte of f as msvcrt.locking LK_LOCK does, ten attempts one second apart before EDEADLOCK
func LockFile(f *os.File) error {
	handle := windows.Handle(f.Fd())
	for attempt := 0; ; attempt++ {
		var overlapped windows.Overlapped
		err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) || attempt == 9 {
			return &os.PathError{Op: "locking", Path: f.Name(), Err: errors.New("Resource deadlock avoided")}
		}
		time.Sleep(lockRetryDelay)
	}
}

// Unlocks the first byte of f as msvcrt.locking LK_UNLCK does
func UnlockFile(f *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped); err != nil {
		return &os.PathError{Op: "locking", Path: f.Name(), Err: errors.New("Permission denied")}
	}
	return nil
}
