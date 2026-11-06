//go:build windows

package agent

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// One byte lock at offset zero retried each second for ten attempts like msvcrt LK_LOCK, or its release
func lockAppendHandle(f *os.File, acquire bool) error {
	h := windows.Handle(f.Fd())
	if !acquire {
		return windows.UnlockFileEx(h, 0, 1, 0, &windows.Overlapped{})
	}
	var err error
	for attempt := range 10 {
		err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
		if err == nil {
			return nil
		}
		if attempt < 9 {
			time.Sleep(time.Second)
		}
	}
	return err
}
