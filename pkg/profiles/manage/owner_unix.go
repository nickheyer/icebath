//go:build !windows

package manage

import (
	"os"
	"syscall"
)

// Owner uid of a path, false where the platform has no uid
func fileOwnerUID(path string) (int, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(sys.Uid), true
}
