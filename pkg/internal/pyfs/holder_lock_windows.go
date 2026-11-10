//go:build windows

package pyfs

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Released lock files keep one space since a byte range lock needs a non empty file
const holderReleasePad = " "

// Seeds an empty lock file with one byte and opens it read write without creating, as open r+ does
func openHolderLockFile(path string) (*os.File, error) {
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		_ = os.WriteFile(path, []byte(" "), 0o666)
	}
	return os.OpenFile(path, os.O_RDWR, 0)
}

// One byte exclusive range lock at offset zero without waiting, or its release
func holderKernelLock(f *os.File, acquire bool) error {
	h := windows.Handle(f.Fd())
	if acquire {
		return windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	}
	return windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
}

// Reports whether a range lock failure is the EACCES msvcrt reports for a held lock
func isHolderLockContention(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_IO_PENDING)
}

// Windows holders are never probed for liveness, a stamped pid always names the holder
func holderGone(int) bool { return false }
