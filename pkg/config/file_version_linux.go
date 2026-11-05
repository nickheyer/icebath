//go:build linux

package config

import (
	"os"
	"syscall"
)

// Inode and change time in nanoseconds of a stat result
func statInoCtime(fi os.FileInfo) (int64, int64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fi.ModTime().UnixNano()
	}
	return int64(st.Ino), st.Ctim.Sec*1e9 + st.Ctim.Nsec
}
