//go:build !windows

package botdesktop

import (
	"os"
	"syscall"
)

// Nil where the host has flock
func flockAvailable() error { return nil }

// Takes an exclusive flock on the open file without waiting for another holder
func flockExclusiveNonBlocking(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
