//go:build windows

package pm

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Tries the exclusive one byte lock at offset zero once, false while another handle holds it
func tryLockFile(f *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return false, nil
	}
	return false, err
}

// Releases the one byte lock at offset zero
func unlockFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}

type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// Reads the reparse tag of path itself and reports a mount point
func reparseIsMountPoint(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var info fileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return false
	}
	return info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && info.ReparseTag == windows.IO_REPARSE_TAG_MOUNT_POINT
}

// Win32 errors that Python maps onto EACCES, EPERM or EBUSY for a held file
func isHeldError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return isAccessError(err) || errno == windows.ERROR_BUSY || errno == syscall.EBUSY
}

// Win32 errors that Python raises as PermissionError
func isAccessError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case windows.ERROR_ACCESS_DENIED, windows.ERROR_CURRENT_DIRECTORY, windows.ERROR_WRITE_PROTECT,
		windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION, windows.ERROR_SHARING_BUFFER_EXCEEDED,
		windows.ERROR_CANNOT_MAKE, windows.ERROR_DRIVE_LOCKED, windows.ERROR_NOT_LOCKED, windows.ERROR_LOCK_FAILED,
		syscall.EACCES, syscall.EPERM:
		return true
	}
	return false
}

// Removes a file, or a directory symlink or junction, the way os.unlink does, refusing a real directory
func unlinkPath(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return &fs.PathError{Op: "unlink", Path: path, Err: err}
	}
	directoryLink := false
	if attrs, aerr := windows.GetFileAttributes(p); aerr == nil && attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		var data windows.Win32finddata
		if h, ferr := windows.FindFirstFile(p, &data); ferr == nil {
			windows.FindClose(h)
			directoryLink = data.Reserved0 == windows.IO_REPARSE_TAG_SYMLINK || data.Reserved0 == windows.IO_REPARSE_TAG_MOUNT_POINT
		}
	}
	if directoryLink {
		err = windows.RemoveDirectory(p)
	} else {
		err = windows.DeleteFile(p)
	}
	if err != nil {
		return &fs.PathError{Op: "unlink", Path: path, Err: err}
	}
	return nil
}
