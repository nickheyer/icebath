//go:build windows

package config

import (
	"io/fs"
	"syscall"
)

// Inode, always zero on Windows, and the creation time in nanoseconds of a stat result
func statIdentity(fi fs.FileInfo) (uint64, int64) {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return 0, fi.ModTime().UnixNano()
	}
	return 0, d.CreationTime.Nanoseconds()
}
