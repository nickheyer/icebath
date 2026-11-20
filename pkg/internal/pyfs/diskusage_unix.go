//go:build !windows

package pyfs

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// Total, used and free bytes of the volume holding path the way shutil.disk_usage reports them on POSIX
func DiskUsage(path string) (total, used, free uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, 0, &fs.PathError{Op: "statvfs", Path: path, Err: err}
	}
	frsize := statvfsFragmentSize(&st)
	total = uint64(st.Blocks) * frsize
	free = uint64(st.Bavail) * frsize
	used = (uint64(st.Blocks) - uint64(st.Bfree)) * frsize
	return total, used, free, nil
}

// Whether the real user may open path with every R_OK, W_OK and X_OK bit of mode, as os.access answers
func Access(path string, mode uint32) bool {
	return unix.Access(path, mode) == nil
}
