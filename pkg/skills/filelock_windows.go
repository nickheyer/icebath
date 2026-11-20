//go:build windows

package skills

import (
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Opens the lock file read write after giving a missing or empty file one space byte to lock
func openSkillLockFile(path string) (*os.File, error) {
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		if err := os.WriteFile(path, []byte(" "), 0o644); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path, os.O_RDWR, 0o644)
}

// Locks the first byte the way msvcrt LK_LOCK does, ten attempts one second apart
func lockSkillFile(f *os.File) (func(), error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
		if err == nil {
			return func() { _ = windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped)) }, nil
		}
		if attempt < 9 {
			time.Sleep(time.Second)
		}
	}
	return nil, err
}
