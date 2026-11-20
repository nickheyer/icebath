//go:build unix

package pyfs

import (
	"os"

	"golang.org/x/sys/unix"
)

// Device and inode numbers os.lstat reports for a path, a final link not followed
func LstatIdentity(path string) (dev, ino uint64, err error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return 0, 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}
