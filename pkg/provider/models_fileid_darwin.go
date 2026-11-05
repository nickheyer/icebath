//go:build darwin

package provider

import (
	"os"
	"syscall"
)

// Inode and inode change time from st_ctimespec, the field Python reports as st_ctime_ns on macOS
func fileIdentity(_ string, st os.FileInfo) (int64, int64) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int64(sys.Ino), sys.Ctimespec.Sec*1e9 + sys.Ctimespec.Nsec
	}
	return 0, 0
}
