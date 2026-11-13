//go:build windows

package updater

import (
	"os"

	"golang.org/x/sys/windows"
)

// NTFS file index of a path as libuv reports it for ino
func fileIno(path string, _ os.FileInfo) float64 {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(handle, &info) != nil {
		return 0
	}
	return float64(uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow))
}
