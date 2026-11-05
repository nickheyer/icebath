//go:build windows

package config

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Takes the cross process .env write lock, failing once the timeout passes
func lockEnvFile(path string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	deadline := time.Now().Add(timeout)
	for {
		ol := new(windows.Overlapped)
		err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return func() { windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped)); f.Close() }, nil
		}
		if err != windows.ERROR_LOCK_VIOLATION {
			f.Close()
			return func() {}, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("Timed out waiting for the environment file lock (%s)", path[:len(path)-len(".lock")])
		}
		time.Sleep(50 * time.Millisecond)
	}
}
