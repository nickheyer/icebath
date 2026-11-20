//go:build !windows

package gatewayrun

import (
	"os"
	"syscall"
)

// Owner uid of a stat result
func statUID(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
