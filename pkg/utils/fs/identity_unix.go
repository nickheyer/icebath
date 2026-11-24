//go:build unix

package fsutil

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// Device and inode numbers of path, a final symbolic link not followed
func Identity(path string) (dev, ino uint64, err error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return 0, 0, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}
