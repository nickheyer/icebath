//go:build windows

package fsutil

import (
	"errors"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Total, used and free bytes of the volume holding path, free counting what the calling user may still use
func DiskUsage(path string) (total, used, free uint64, err error) {
	target := path
	for {
		p, err := windows.UTF16PtrFromString(target)
		if err != nil {
			return 0, 0, 0, &fs.PathError{Op: "diskusage", Path: path, Err: err}
		}
		var avail, totalBytes uint64
		err = windows.GetDiskFreeSpaceEx(p, &avail, &totalBytes, nil)
		if errors.Is(err, windows.ERROR_DIRECTORY) && filepath.Dir(target) != target {
			target = filepath.Dir(target)
			continue
		}
		if err != nil {
			return 0, 0, 0, &fs.PathError{Op: "diskusage", Path: path, Err: err}
		}
		return totalBytes, totalBytes - avail, avail, nil
	}
}

// Whether path exists and, when mode holds AccessWrite, is a directory or a file without the read only attribute
func Access(path string, mode uint32) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return false
	}
	return mode&AccessWrite == 0 || attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || attrs&windows.FILE_ATTRIBUTE_READONLY == 0
}
