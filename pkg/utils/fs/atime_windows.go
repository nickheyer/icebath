//go:build windows

package fsutil

import (
	"io/fs"
	"syscall"
	"time"
)

// Access time of a file from its attribute data, the modification time when the result carries none
func Atime(fi fs.FileInfo) time.Time {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return fi.ModTime()
	}
	return time.Unix(0, d.LastAccessTime.Nanoseconds())
}
