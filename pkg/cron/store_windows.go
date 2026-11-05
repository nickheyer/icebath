//go:build windows

package cron

import (
	"os"

	"golang.org/x/sys/windows"
)

// Windows files carry no POSIX owner to restore
func fileOwner(st os.FileInfo) (int, int, bool) { return 0, 0, false }

// NTFS file index, the st_ino Python reports on Windows
func fileInode(path string, _ os.FileInfo) uint64 {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) != nil {
		return 0
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
}
