//go:build !windows

package state

import (
	"os"
	"syscall"
)

// Takes the advisory lock on the handle without waiting, failing on contention
func tryLockNonblocking(handle *os.File) error {
	return flockRetrying(handle, syscall.LOCK_EX|syscall.LOCK_NB)
}

// Drops the advisory lock, clearing the holder record when asked, and closes the handle
func releaseLockHandle(handle *os.File, clearRecord bool) {
	if clearRecord {
		clearLockHolderRecord(handle)
	}
	_ = flockRetrying(handle, syscall.LOCK_UN)
	_ = handle.Close()
}

// Bounded repair lock acquire that breaks a lock only an orphaned descriptor of a dead holder keeps
func acquireRepairLock(lockPath string, handle *os.File, timeout float64) (lockOutcome, *os.File, error) {
	return acquireDBFlock(lockPath, handle, timeout, repairLockPollSeconds, "state.db repair lock")
}

// Holder record the repair lock file carries
func recordedRepairLockHolder(handle *os.File) pyObject { return readLockHolderRecord(handle) }
