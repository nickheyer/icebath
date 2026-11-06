//go:build windows

package pyfs

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// One msvcrt.locking LK_NBLCK attempt on the first byte, the OSError Python raises when another handle holds it
func TryLockFile(f *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err == nil {
		return true, nil
	}
	return false, &os.PathError{Op: "locking", Path: f.Name(), Err: errors.New("Permission denied")}
}
