//go:build !windows

package files

import (
	"io/fs"
	"os"
	"syscall"
)

// identityInfo is a stat answer that states its own device and inode
type identityInfo interface {
	Identity() (dev, ino uint64)
}

// Device and inode of a stat answer
func fileIdentity(st fs.FileInfo, _ string) (uint64, uint64, bool) {
	if id, ok := st.(identityInfo); ok {
		dev, ino := id.Identity()
		return dev, ino, true
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(sys.Dev), uint64(sys.Ino), true
}

// Hard link count of a stat answer
func fileLinks(st fs.FileInfo, _ string) uint64 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Nlink)
	}
	return 1
}

// Opens without following a final link and without blocking on a FIFO or device
func openNoFollow(p string, write bool) (*os.File, error) {
	flags := os.O_RDONLY
	if write {
		flags = os.O_RDWR
	}
	return os.OpenFile(p, flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
