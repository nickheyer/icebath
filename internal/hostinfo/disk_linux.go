package hostinfo

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// Space of the volume holding path, free being what an unprivileged user may still fill
func DiskUsage(path string) (Disk, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Disk{}, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	block := uint64(st.Frsize)
	if block == 0 {
		block = uint64(st.Bsize)
	}
	return Disk{Total: st.Blocks * block, Used: (st.Blocks - st.Bfree) * block, Free: st.Bavail * block}, nil
}
