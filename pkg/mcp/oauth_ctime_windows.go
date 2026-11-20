//go:build windows

package mcp

import (
	"os"
	"syscall"
)

// Creation time of a stat result in nanoseconds, the Windows st_ctime
func fileCtimeNs(st os.FileInfo) int64 {
	if sys, ok := st.Sys().(*syscall.Win32FileAttributeData); ok {
		return sys.CreationTime.Nanoseconds()
	}
	return 0
}
