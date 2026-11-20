//go:build unix

package gateway

import (
	"os"

	"golang.org/x/sys/unix"
)

// Hard link count os.lstat reports for a path, a final link not followed
func lstatNlink(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	return uint64(st.Nlink), nil
}
