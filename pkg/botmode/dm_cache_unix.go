//go:build unix

package botmode

import (
	"io/fs"
	"os"
	"syscall"
)

// Real user id of this process
func currentUID() (int, bool) { return os.Getuid(), true }

// Owning user id of a stat result
func ownerUID(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
