package catalog

import (
	"io/fs"
	"syscall"
)

func statIdentity(path string, info fs.FileInfo) (inode uint64, changed int64) {
	changed = info.ModTime().UnixNano()
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		changed = data.CreationTime.Nanoseconds()
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, changed
	}
	handle, err := syscall.CreateFile(name, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, changed
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &data); err != nil {
		return 0, changed
	}
	return uint64(data.FileIndexHigh)<<32 | uint64(data.FileIndexLow), changed
}
