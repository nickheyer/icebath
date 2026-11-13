//go:build windows

package fileutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Total, used and free bytes of the volume holding path, a file path measured through its directory
func DiskUsage(path string) (DiskSpace, error) {
	target := path
	for {
		p, err := windows.UTF16PtrFromString(target)
		if err != nil {
			return DiskSpace{}, &fs.PathError{Op: "diskusage", Path: path, Err: err}
		}
		var avail, total, free uint64
		err = windows.GetDiskFreeSpaceEx(p, &avail, &total, &free)
		if errors.Is(err, windows.ERROR_DIRECTORY) && filepath.Dir(target) != target {
			target = filepath.Dir(target)
			continue
		}
		if err != nil {
			return DiskSpace{}, &fs.PathError{Op: "diskusage", Path: path, Err: err}
		}
		return DiskSpace{Total: total, Used: total - free, Free: free}, nil
	}
}

// Reports whether path exists and, for a write, is a directory or a file without the read-only attribute
func Accessible(path string, a Access) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return false
	}
	return a&AccessWrite == 0 || attrs&windows.FILE_ATTRIBUTE_READONLY == 0 || attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0
}

// Volume serial and file index of path itself, a final reparse point not followed
func LstatID(path string) (FileID, error) {
	id, _, err := handleIdentity(path)
	return id, err
}

// Volume serial, file index and hard link count of the file at path
func linkIdentity(path string, _ fs.FileInfo) (FileID, uint64, error) {
	return handleIdentity(path)
}

// Identity and link count read through a handle opened on path itself
func handleIdentity(path string) (FileID, uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return FileID{}, 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return FileID{}, 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return FileID{}, 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	id := FileID{Device: uint64(info.VolumeSerialNumber), Inode: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)}
	return id, uint64(info.NumberOfLinks), nil
}
