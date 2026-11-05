//go:build !windows

package mcp

import (
	"os"
	"syscall"
)

// Inode number of a stat result
func fileInode(_ string, st os.FileInfo) uint64 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Ino)
	}
	return 0
}
