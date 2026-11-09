//go:build windows

package selfupdate

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Whether a LockFileEx failure means another process holds the byte
func claimHeldErrno(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING)
}

// Takes or drops a non blocking lock on the first byte, false only while another launch holds it
func lockClaimFile(f *os.File, lock bool) bool {
	handle := windows.Handle(f.Fd())
	overlapped := windows.Overlapped{}
	if lock {
		if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
			return !claimHeldErrno(err)
		}
		return true
	}
	windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
	return true
}

// Lock attempt of the ZIP swap owner lock, held true for another live owner and the error for a lock that cannot be taken
func tryZipSwapLock(f *os.File) (held bool, err error) {
	overlapped := windows.Overlapped{}
	lockErr := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if lockErr == nil {
		return false, nil
	}
	if claimHeldErrno(lockErr) {
		return true, nil
	}
	return false, lockErr
}

// Volume serial and file index of an entry without following a link, the identity Python's lstat reports on Windows
func fileIdentity(path string, info os.FileInfo) (dev, ino uint64, ok bool) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, 0, false
	}
	defer windows.CloseHandle(handle)
	var data windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &data); err != nil {
		return 0, 0, false
	}
	return uint64(data.VolumeSerialNumber), uint64(data.FileIndexHigh)<<32 | uint64(data.FileIndexLow), true
}

// Modification time of an entry in nanoseconds
func mtimeNanos(info os.FileInfo) int64 { return info.ModTime().UnixNano() }

// Link count of an entry, one when the platform does not report it
func linkCountOf(os.FileInfo) uint64 { return 1 }
