//go:build windows

package fsutil

import "golang.org/x/sys/windows"

// Tag of the reparse point at path, not followed, zero for a plain file or an unreadable path
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

// Whether path is an NTFS junction, which a tree copy descends into rather than copying as a link
func isMountPoint(path string) bool { return ReparseTag(path) == windows.IO_REPARSE_TAG_MOUNT_POINT }
