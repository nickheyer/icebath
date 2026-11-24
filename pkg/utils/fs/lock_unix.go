//go:build !windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// Blocks until f holds an exclusive advisory lock on its whole file, a flock retried through interrupted waits
func LockFile(f *os.File) error { return flock(f, unix.LOCK_EX) }

// Releases the lock f holds
func UnlockFile(f *os.File) error { return flock(f, unix.LOCK_UN) }

// One attempt at the exclusive lock without waiting, false and no error when another handle holds it
func TryLockFile(f *os.File) (bool, error) {
	err := flock(f, unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK), errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EACCES):
		return false, nil
	}
	return false, err
}

func flock(f *os.File, how int) error {
	for {
		err := unix.Flock(int(f.Fd()), how)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EINTR) {
			return &fs.PathError{Op: "flock", Path: f.Name(), Err: err}
		}
	}
}
