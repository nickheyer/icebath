//go:build windows

package cron

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Locks the first byte like msvcrt LK_LOCK, retrying once a second for ten attempts before raising
func acquireFlock(f *os.File, timeout time.Duration) (bool, error) {
	h := windows.Handle(f.Fd())
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
		if err == nil {
			return true, nil
		}
		time.Sleep(time.Second)
	}
	return false, &os.PathError{Op: "locking", Path: f.Name(), Err: err}
}

// Unlocks the first byte and closes the lock file, ignoring unlock errors
func releaseFlock(f *os.File) {
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
	f.Close()
}
