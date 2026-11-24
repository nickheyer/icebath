//go:build netbsd || solaris

package fsutil

import "golang.org/x/sys/unix"

// Block counts of the volume holding path from statvfs, counted in fragments, the block size standing in where the fragment size is zero
func volumeOf(path string) (volumeBlocks, error) {
	var st unix.Statvfs_t
	if err := unix.Statvfs(path, &st); err != nil {
		return volumeBlocks{}, err
	}
	unit := uint64(st.Frsize)
	if unit == 0 {
		unit = uint64(st.Bsize)
	}
	return volumeBlocks{blocks: uint64(st.Blocks), free: uint64(st.Bfree), avail: uint64(st.Bavail), unit: unit}, nil
}
