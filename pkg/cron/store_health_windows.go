package cron

import (
	"golang.org/x/sys/windows"
)

// Write access the way os.access W_OK reports it on Windows, false only for a read only file
func writable(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return false
	}
	return attrs&windows.FILE_ATTRIBUTE_READONLY == 0 || attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0
}

// Total free bytes on the volume the way shutil.disk_usage reports them
func freeBytes(path string) (uint64, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, false
	}
	return free, true
}
