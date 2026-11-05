//go:build !windows

package profiles

import (
	"os"
	"syscall"
)

// Inode number of a stat result, zero when the platform does not report one
func fileInode(_ string, st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Ino)
	}
	return 0
}
