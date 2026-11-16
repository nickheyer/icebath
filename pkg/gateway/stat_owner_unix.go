//go:build !windows

package gateway

import (
	"os"
	"syscall"
)

// Owner uid of a stat result
func statOwnerUID(st os.FileInfo) int { return int(st.Sys().(*syscall.Stat_t).Uid) }
