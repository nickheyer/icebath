//go:build windows

package webhooksubs

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Locks the first byte like msvcrt LK_LOCK, retrying once a second for ten attempts
func lockFile(f *os.File) error {
	h := windows.Handle(f.Fd())
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
		if err == nil {
			return nil
		}
	}
	return err
}

// Unlocks the first byte
func unlockFile(f *os.File) {
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
