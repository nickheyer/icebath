//go:build !windows

package backend

import (
	"os"
	"syscall"
)

// Whether a file belongs to a uid other than this process
func ownedByOtherUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) != os.Getuid()
}
