//go:build !windows

package gateway

import (
	"os"
	"syscall"
)

// Owner uid of a path, false when it cannot be stat'ed
func fileOwnerUID(p string) (int, bool) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
