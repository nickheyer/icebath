//go:build !windows

package pyzip

import (
	"os"
	"syscall"
)

// Raw st_mode of a stat result
func statMode(st os.FileInfo) uint32 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint32(sys.Mode)
	}
	return uint32(st.Mode().Perm())
}
