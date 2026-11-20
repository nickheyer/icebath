//go:build !windows

package pyfs

import (
	"os"
	"syscall"
)

// All twelve permission bits of the file path names, false when it is missing or cannot be read
func PreserveFileMode(path string) (uint32, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, false
	}
	return uint32(st.Mode) & 0o7777, true
}

// Owner of the file path names, nil when it cannot be read
func PreserveFileOwner(path string) *FileOwner {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return nil
	}
	return &FileOwner{UID: int(st.Uid), GID: int(st.Gid)}
}

func chownPath(path string, uid, gid int) { _ = os.Chown(path, uid, gid) }

func chmodPath(path string, mode uint32) { _ = syscall.Chmod(path, mode&0o7777) }
