//go:build linux

package config

import (
	"io/fs"
	"syscall"
)

// Inode and status change time in nanoseconds of a stat result
func statIdentity(fi fs.FileInfo) (uint64, int64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fi.ModTime().UnixNano()
	}
	return st.Ino, st.Ctim.Nano()
}
