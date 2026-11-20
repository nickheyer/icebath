//go:build !windows

package webhooksubs

import (
	"os"
	"syscall"
)

// Blocks until the exclusive whole file flock is held
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

// Releases the whole file flock
func unlockFile(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
