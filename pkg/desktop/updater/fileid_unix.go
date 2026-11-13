//go:build !windows

package updater

import (
	"os"
	"syscall"
)

// Inode number of a stat result
func fileIno(_ string, info os.FileInfo) float64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return float64(st.Ino)
	}
	return 0
}
