//go:build windows

package fsx

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// Exit code GetExitCodeProcess reports for a process still running
const stillActive = 259

// Windows follows no planted link when opening a lock file
const noFollow = 0

// One attempt at the exclusive lock of the first byte without waiting, false and no error when another handle holds it
func tryLock(f *os.File) (bool, error) {
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
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return false, nil
	}
	return false, &fs.PathError{Op: "lock", Path: f.Name(), Err: err}
}

// Releases the lock f holds
func unlock(f *os.File) error {
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped)); err != nil {
		return &fs.PathError{Op: "unlock", Path: f.Name(), Err: err}
	}
	return nil
}

// Whether no process with the pid is running, one this process may not open still counting as alive
func processGone(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code != stillActive
}

// Whether a failure is another program, usually an antivirus scan, holding the file for a while
func Held(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case windows.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION, windows.ERROR_BUSY, syscall.EACCES, syscall.EPERM,
		syscall.EBUSY:
		return true
	}
	return false
}

// Whether the path is a regular file, which Windows runs whatever its bits
func Executable(path string) bool { return IsFile(path) }

// Windows flushes a rename with the file
func syncDir(string) {}
