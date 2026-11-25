//go:build windows

package credentials

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// Releases the lock f holds
func unlockFile(f *os.File) error {
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped)); err != nil {
		return &fs.PathError{Op: "unlock", Path: f.Name(), Err: err}
	}
	return nil
}

// One attempt at the exclusive lock on the first byte without waiting, false and no error when another handle holds it
func tryLockFile(f *os.File) (bool, error) {
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		var done uint32
		err = windows.GetOverlappedResult(h, ol, &done, true)
	}
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return false, nil
	}
	return false, &fs.PathError{Op: "lock", Path: f.Name(), Err: err}
}
