//go:build darwin || freebsd || dragonfly

package fsutil

import "golang.org/x/sys/unix"

// Block counts of the volume holding path in its fundamental block size, a negative available count of a volume eaten into its reserve read as none
func volumeOf(path string) (volumeBlocks, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return volumeBlocks{}, err
	}
	return volumeBlocks{
		blocks: uint64(max(st.Blocks, 0)),
		free:   uint64(max(st.Bfree, 0)),
		avail:  uint64(max(st.Bavail, 0)),
		unit:   uint64(st.Bsize),
	}, nil
}
