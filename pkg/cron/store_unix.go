//go:build !windows

package cron

import (
	"os"
	"syscall"
)

// Owner uid and gid of a stat result
func fileOwner(st os.FileInfo) (int, int, bool) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(sys.Uid), int(sys.Gid), true
}

// Inode number of a stat result
func fileInode(_ string, st os.FileInfo) uint64 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Ino)
	}
	return 0
}
