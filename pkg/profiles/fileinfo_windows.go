//go:build windows

package profiles

import (
	"os"
	"syscall"
)

// File index of path as Python reports st_ino on Windows, zero when the handle cannot be opened
func fileInode(path string, _ os.FileInfo) uint64 {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	h, err := syscall.CreateFile(p, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return 0
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
}
