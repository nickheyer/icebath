//go:build windows

package fsutil

import (
	"io/fs"

	"golang.org/x/sys/windows"
)

// Volume serial number and file index of path, standing for device and inode, a final reparse point not followed
func Identity(path string) (dev, ino uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, 0, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, 0, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	return uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), nil
}
