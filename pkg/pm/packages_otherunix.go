//go:build unix && !linux && !darwin && !freebsd

package pm

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Last access time the inode records, read through the uniform unix stat layout
func fileAccessTime(path string, st os.FileInfo) (time.Time, error) {
	var raw unix.Stat_t
	if err := unix.Stat(path, &raw); err != nil {
		return time.Time{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return time.Unix(int64(raw.Atim.Sec), int64(raw.Atim.Nsec)), nil
}
