//go:build windows

package browser

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const oNoFollow = 0

func init() { ntLockContended[windows.ERROR_LOCK_VIOLATION] = true }

func pidNamespaceInode() (uint64, bool) { return 0, false }

// Locks the first byte like msvcrt.locking, retrying contention once a second until the deadline
func lockFile(f *os.File, records string, blocking bool) (bool, error) {
	deadline := time.Now().Add(ntLockTimeout)
	for {
		ol := new(windows.Overlapped)
		err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return true, nil
		}
		failure := ntLockFailure(err, records, !blocking || !time.Now().Before(deadline))
		if failure == err {
			return false, err
		}
		if !blocking {
			return false, nil
		}
		if failure != nil {
			return false, failure
		}
		time.Sleep(time.Second)
	}
}

func unlockFile(f *os.File) {
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
