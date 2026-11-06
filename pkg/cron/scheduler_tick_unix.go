//go:build !windows

package cron

import (
	"os"
	"syscall"
)

// Tries one nonblocking exclusive flock and returns the raw errno on contention or failure
func tryFlockOnce(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	return nil
}
