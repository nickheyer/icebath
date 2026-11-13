//go:build windows

package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"golang.org/x/sys/windows"
)

// Python labels Windows OS errors with the WinError code
const errnoLabel = "WinError"

// Byte range lock on the lock sibling retried ten times a second apart like msvcrt LK_LOCK
func fileLock(ctx context.Context, path string) (func(), error) {
	lockPath := path + ".lock"
	if _, err := home.Mkdir(ctx, filepath.Dir(lockPath)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		f.Close()
		return nil, err
	}
	h := windows.Handle(f.Fd())
	for attempt := 0; ; attempt++ {
		ol := new(windows.Overlapped)
		err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			break
		}
		if attempt == 9 {
			f.Close()
			return nil, &pyError{class: "OSError", msg: "[Errno 36] Resource deadlock avoided"}
		}
		time.Sleep(time.Second)
	}
	return func() {
		windows.UnlockFileEx(h, 0, 1, 0, new(windows.Overlapped))
		f.Close()
	}, nil
}

// Text mode writes translate LF to CRLF on Windows
func platformNewlines(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
