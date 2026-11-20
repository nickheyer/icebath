//go:build !windows && !darwin

package provider

import (
	"os"
	"syscall"
)

// Inode and inode change time of a stat result for the catalog fingerprint
func fileIdentity(_ string, st os.FileInfo) (int64, int64) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int64(sys.Ino), sys.Ctim.Sec*1e9 + sys.Ctim.Nsec
	}
	return 0, 0
}
