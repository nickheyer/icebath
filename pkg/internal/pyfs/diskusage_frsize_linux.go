package pyfs

import "golang.org/x/sys/unix"

// Fragment size statvfs reports, the block size when the kernel leaves it zero
func statvfsFragmentSize(st *unix.Statfs_t) uint64 {
	if st.Frsize != 0 {
		return uint64(st.Frsize)
	}
	return uint64(st.Bsize)
}
