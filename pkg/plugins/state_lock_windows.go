//go:build windows

package plugins

import (
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Locks the first byte the way msvcrt LK_LOCK does, ten attempts one second apart
func lockStateFile(handle *os.File) (func(), error) {
	if end, err := handle.Seek(0, io.SeekEnd); err == nil && end == 0 {
		if _, err := handle.Write([]byte{0}); err != nil {
			return nil, err
		}
	}
	h := windows.Handle(handle.Fd())
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		ol := new(windows.Overlapped)
		err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return func() { _ = windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped)) }, nil
		}
		if attempt < 9 {
			time.Sleep(time.Second)
		}
	}
	return nil, err
}
