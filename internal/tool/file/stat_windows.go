package file

import (
	"os"
	"syscall"
)

func fileStamp(path string, fi os.FileInfo) (uint64, uint64, int64, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, 0, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	share := uint32(syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE)
	handle, err := syscall.CreateFile(name, 0, share, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, 0, 0, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	defer syscall.CloseHandle(handle)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, 0, 0, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	index := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return uint64(info.VolumeSerialNumber), index, info.CreationTime.Nanoseconds(), nil
}
