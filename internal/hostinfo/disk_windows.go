package hostinfo

import (
	"errors"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Space of the volume holding path, a file path answered for its directory, free counting every free byte
func DiskUsage(path string) (Disk, error) {
	target := path
	for {
		name, err := windows.UTF16PtrFromString(target)
		if err != nil {
			return Disk{}, &fs.PathError{Op: "GetDiskFreeSpaceEx", Path: path, Err: err}
		}
		var available, total, free uint64
		err = windows.GetDiskFreeSpaceEx(name, &available, &total, &free)
		if errors.Is(err, windows.ERROR_DIRECTORY) && filepath.Dir(target) != target {
			target = filepath.Dir(target)
			continue
		}
		if err != nil {
			return Disk{}, &fs.PathError{Op: "GetDiskFreeSpaceEx", Path: path, Err: err}
		}
		return Disk{Total: total, Used: total - free, Free: free}, nil
	}
}
