//go:build windows

package state

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Windows errors the C runtime maps to EACCES or EAGAIN, the errno msvcrt locking contention raises
func lockContentionErrno(errno syscall.Errno) bool {
	switch errno {
	case syscall.EAGAIN, syscall.EALREADY, syscall.EINPROGRESS, syscall.EACCES, syscall.EWOULDBLOCK,
		windows.ERROR_ACCESS_DENIED, windows.ERROR_CURRENT_DIRECTORY, windows.ERROR_NETWORK_ACCESS_DENIED,
		windows.ERROR_DRIVE_LOCKED, windows.ERROR_SEEK_ON_DEVICE, windows.ERROR_NOT_LOCKED, windows.ERROR_LOCK_FAILED,
		windows.ERROR_FAIL_I24, windows.ERROR_NO_PROC_SLOTS, windows.ERROR_MAX_THRDS_REACHED, windows.ERROR_NESTING_NOT_ALLOWED:
		return true
	}
	return errno >= windows.ERROR_WRITE_PROTECT && errno <= windows.ERROR_SHARING_BUFFER_EXCEEDED
}

// Process existence probe through OpenProcess, a failed open reading as an indeterminate OSError
func signalZero(pid int) (alive, gone bool) {
	h, err := windows.OpenProcess(windows.PROCESS_ALL_ACCESS, false, uint32(pid))
	if err != nil {
		return false, false
	}
	_ = windows.CloseHandle(h)
	return true, false
}

// Volume serial number and file index of a path
func statFileIdentity(path string) (FileIdentity, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return FileIdentity{}, false
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return FileIdentity{}, false
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return FileIdentity{}, false
	}
	return FileIdentity{Dev: uint64(info.VolumeSerialNumber), Ino: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)}, true
}

// Bounded non blocking lock of the first byte, the msvcrt locking contract without an orphan break
func acquireMsvcrtLock(lockPath string, handle *os.File, timeout float64) lockOutcome {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for {
		ol := new(windows.Overlapped)
		err := windows.LockFileEx(windows.Handle(handle.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return lockAcquired
		}
		if !IsAdvisoryLockContention(err) {
			slog.Warn(fmt.Sprintf("Could not acquire FTS rebuild lock %s (%s) — deferring on a non-contention error.", lockPath, err))
			return lockFailed
		}
		if !time.Now().Before(deadline) {
			return lockTimedOut
		}
		time.Sleep(time.Duration(ftsRebuildLockPollSeconds * float64(time.Second)))
	}
}

// Takes the FTS rebuild authority through a first byte lock
func acquireAdmissionLock(lockPath string, handle *os.File, timeout float64) (lockOutcome, *os.File, error) {
	return acquireMsvcrtLock(lockPath, handle, timeout), handle, nil
}

// Unlocks the first byte, ignoring failures
func releaseAdmissionLock(handle *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(handle.Fd()), 0, 1, 0, new(windows.Overlapped))
}
