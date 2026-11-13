package state

import (
	"os"

	"golang.org/x/sys/windows"
)

// Volume serial and file index of the file path names, reparse points followed
func identityOf(path string) (fileIdentity, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fileIdentity{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return fileIdentity{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fileIdentity{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return fileIdentity{dev: uint64(info.VolumeSerialNumber), ino: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)}, nil
}
