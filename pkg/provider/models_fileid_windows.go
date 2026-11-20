//go:build windows

package provider

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// File index and creation time like Python os.stat, index zero when the file cannot be opened
func fileIdentity(path string, st os.FileInfo) (int64, int64) {
	var created int64
	if sys, ok := st.Sys().(*syscall.Win32FileAttributeData); ok {
		created = sys.CreationTime.Nanoseconds()
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, created
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, created
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, created
	}
	return int64(uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)), created
}
