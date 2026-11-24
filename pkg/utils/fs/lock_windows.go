//go:build windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// Blocks until f holds an exclusive lock on its first byte, the one byte every byte range lock of this program takes on Windows, so it excludes the LockFileEx locks at offset zero other packages take on the same file; other handles cannot read or write that byte while it is held
func LockFile(f *os.File) error {
	if err := lockFirstByte(f, windows.LOCKFILE_EXCLUSIVE_LOCK); err != nil {
		return &fs.PathError{Op: "lock", Path: f.Name(), Err: err}
	}
	return nil
}

// Releases the lock f holds
func UnlockFile(f *os.File) error {
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped)); err != nil {
		return &fs.PathError{Op: "unlock", Path: f.Name(), Err: err}
	}
	return nil
}

// One attempt at the exclusive lock without waiting, false and no error when another handle holds it
func TryLockFile(f *os.File) (bool, error) {
	err := lockFirstByte(f, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return false, nil
	}
	return false, &fs.PathError{Op: "lock", Path: f.Name(), Err: err}
}

// Locks byte zero, a region past the end of the file included, waiting for the outcome when the handle was opened for overlapped I/O
func lockFirstByte(f *os.File, flags uint32) error {
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(h, flags, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		var done uint32
		err = windows.GetOverlappedResult(h, ol, &done, true)
	}
	return err
}
