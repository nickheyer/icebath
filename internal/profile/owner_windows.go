//go:build windows

package profile

import (
	"math"

	"golang.org/x/sys/windows"
)

// Windows files have no user id to compare, so ownership is never refused
func ownerOf(string) (int, bool) {
	return 0, false
}

// Bytes a user may still store on the volume of the folder, negative when the volume tells nothing
func room(dir string) int64 {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return -1
	}
	var free, total, all uint64
	if windows.GetDiskFreeSpaceEx(path, &free, &total, &all) != nil || total == 0 {
		return -1
	}
	return int64(min(free, math.MaxInt64))
}
