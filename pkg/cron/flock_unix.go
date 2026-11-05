//go:build !windows

package cron

import (
	"os"
	"syscall"
	"time"
)

// Polls a nonblocking exclusive flock every 100ms, false once the timeout passes without the lock
func acquireFlock(f *os.File, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Unlocks and closes a lock file, ignoring unlock errors
func releaseFlock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}
