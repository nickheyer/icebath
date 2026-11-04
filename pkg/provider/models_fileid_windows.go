//go:build windows

package provider

import (
	"os"
	"syscall"
)

// Creation time of a stat result for the catalog fingerprint, inode unavailable on Windows
func fileIdentity(st os.FileInfo) (int64, int64) {
	if sys, ok := st.Sys().(*syscall.Win32FileAttributeData); ok {
		return 0, sys.CreationTime.Nanoseconds()
	}
	return 0, 0
}
