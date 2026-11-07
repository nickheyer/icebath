//go:build !windows

package logging

import (
	"os"
	"syscall"
)

// Owner uid and gid a file carried
func fileOwner(st os.FileInfo) (int, int, bool) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(s.Uid), int(s.Gid), true
}
