//go:build windows

package pm

import "syscall"

// Sets access and modification time to one instant through SetFileTime with the FILETIME arithmetic os.utime uses
func tarSetFileTimes(path string, sec, nsec int64) error {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(name, syscall.FILE_WRITE_ATTRIBUTES, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	ticks := (sec+11644473600)*10000000 + nsec/100
	ft := syscall.Filetime{LowDateTime: uint32(ticks), HighDateTime: uint32(uint64(ticks) >> 32)}
	return syscall.SetFileTime(h, nil, &ft, &ft)
}
