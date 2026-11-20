//go:build !windows

package pyfs

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Blocks until f holds an exclusive whole file flock, retrying interrupted waits as fcntl.flock does
func LockFile(f *os.File) error {
	return flock(f, unix.LOCK_EX)
}

// Releases the flock f holds
func UnlockFile(f *os.File) error {
	return flock(f, unix.LOCK_UN)
}

// Applies one flock operation, retrying EINTR and wrapping failures as the OSError Python raises
func flock(f *os.File, how int) error {
	for {
		err := unix.Flock(int(f.Fd()), how)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EINTR) {
			return &os.PathError{Op: "flock", Path: f.Name(), Err: err}
		}
	}
}
