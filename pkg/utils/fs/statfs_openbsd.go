package fsutil

import "golang.org/x/sys/unix"

// Block counts of the volume holding path in its fundamental block size, a negative available count of a volume eaten into its reserve read as none
func volumeOf(path string) (volumeBlocks, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return volumeBlocks{}, err
	}
	return volumeBlocks{blocks: st.F_blocks, free: st.F_bfree, avail: uint64(max(st.F_bavail, 0)), unit: uint64(st.F_bsize)}, nil
}
