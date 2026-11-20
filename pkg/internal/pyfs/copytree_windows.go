//go:build windows

package pyfs

import (
	"golang.org/x/sys/windows"
)

// Whether path is an NTFS mount point, which a tree copy descends into rather than copying as a link
func isMountPoint(path string) bool {
	return ReparseTag(path) == windows.IO_REPARSE_TAG_MOUNT_POINT
}

// Reparse tag of a path without following it, zero for a plain file or an unreadable path
func ReparseTag(path string) uint32 {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var data windows.Win32finddata
	h, err := windows.FindFirstFile(p, &data)
	if err != nil {
		return 0
	}
	windows.FindClose(h)
	if data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return 0
	}
	return data.Reserved0
}

// Windows leaves link times untouched since utime cannot skip following links there
func copySymlinkTimes(string, string) error { return nil }
