package state

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Locks the first byte of the handle without waiting, failing on contention
func tryLockNonblocking(handle *os.File) error {
	return windows.LockFileEx(windows.Handle(handle.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped))
}

// Unlocks the first byte and closes the handle, Windows keeping no holder record
func releaseLockHandle(handle *os.File, _ bool) {
	_ = windows.UnlockFileEx(windows.Handle(handle.Fd()), 0, 1, 0, new(windows.Overlapped))
	_ = handle.Close()
}

// Bounded first byte lock acquire, a non contention error failing at once
func acquireRepairLock(lockPath string, handle *os.File, timeout float64) (lockOutcome, *os.File, error) {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for {
		err := tryLockNonblocking(handle)
		if err == nil {
			return lockAcquired, handle, nil
		}
		if !IsAdvisoryLockContention(err) {
			slog.Warn(fmt.Sprintf("Could not acquire state.db repair lock %s (%s) — skipping schema surgery on a "+
				"non-contention error.", lockPath, err))
			return lockFailed, handle, nil
		}
		if !time.Now().Before(deadline) {
			return lockTimedOut, handle, nil
		}
		time.Sleep(time.Duration(repairLockPollSeconds * float64(time.Second)))
	}
}

// Windows locks carry no holder record
func recordedRepairLockHolder(*os.File) pyObject { return nil }
