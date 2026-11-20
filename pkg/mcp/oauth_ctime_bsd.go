//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package mcp

import (
	"os"
	"syscall"
)

// Change time of a stat result in nanoseconds
func fileCtimeNs(st os.FileInfo) int64 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int64(sys.Ctimespec.Sec)*1e9 + int64(sys.Ctimespec.Nsec)
	}
	return 0
}
