//go:build !windows

package browser

import (
	"io/fs"
	"os"
	"syscall"
)

// Real dir owned by this uid with no group or other permission bits
func ownedPrivateDir(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsDir() && int(st.Uid) == os.Getuid() && st.Mode&0o077 == 0
}

// Owned by this uid with no group or other write bits
func ownedWithoutGroupWrite(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid() && st.Mode&0o022 == 0
}

// POSIX hosts have a uid
const harnessHasUID = true

// Uid keying the private short dir digest
func harnessUID() int { return os.Getuid() }
