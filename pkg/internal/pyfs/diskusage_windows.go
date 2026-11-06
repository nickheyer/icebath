package pyfs

import (
	"errors"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Total, used and free bytes of the volume holding path the way shutil.disk_usage reports them on Windows
func DiskUsage(path string) (total, used, free uint64, err error) {
	target := path
	for {
		p, err := windows.UTF16PtrFromString(target)
		if err != nil {
			return 0, 0, 0, err
		}
		var avail, totalBytes uint64
		err = windows.GetDiskFreeSpaceEx(p, &avail, &totalBytes, nil)
		if errors.Is(err, windows.ERROR_DIRECTORY) && filepath.Dir(target) != target {
			target = filepath.Dir(target)
			continue
		}
		if err != nil {
			return 0, 0, 0, &fs.PathError{Op: "disk_usage", Path: path, Err: err}
		}
		return totalBytes, totalBytes - avail, avail, nil
	}
}

// Whether path exists and, for W_OK, is no read only file, the attribute test os.access makes on Windows
func Access(path string, mode uint32) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return false
	}
	return mode&2 == 0 || attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || attrs&windows.FILE_ATTRIBUTE_READONLY == 0
}
