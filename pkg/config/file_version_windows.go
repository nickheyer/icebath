//go:build windows

package config

import (
	"os"
	"syscall"
)

// Zero inode and creation time in nanoseconds, as Python reports them on Windows
func statInoCtime(fi os.FileInfo) (int64, int64) {
	st, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return 0, fi.ModTime().UnixNano()
	}
	return 0, st.CreationTime.Nanoseconds()
}
