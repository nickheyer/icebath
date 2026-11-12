//go:build !windows

package hardening

import (
	"io/fs"
	"os"
	"syscall"
)

// Owner uid of a stat result
func ownerUID(st fs.FileInfo) (int, bool) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(s.Uid), true
	}
	return 0, false
}

// Effective uid of this process as process.getuid answers
func processUID() (int, bool) { return os.Getuid(), true }
