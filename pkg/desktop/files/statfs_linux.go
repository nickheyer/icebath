//go:build linux

package files

import "golang.org/x/sys/unix"

// Kernel pseudo file systems by magic number, wherever they are mounted
var pseudoFSTypes = map[int64]bool{
	0x9fa0: true, 0x62656572: true, 0x64626720: true, 0x74726163: true, 0x73636673: true, 0x27e0eb: true,
	0x63677270: true, 0xcafe4a11: true, 0xde5e81e4: true, 0x62656570: true, 0x6165676c: true, 0xf97cff8c: true,
}

// File system type of the volume holding target
func statfsType(target string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(target, &st); err != nil {
		return 0, err
	}
	return int64(st.Type), nil
}

// True when target lives on a kernel pseudo file system
func onPseudoFilesystem(target string) bool {
	t, err := pfs.statfs(target)
	return err == nil && pseudoFSTypes[t]
}
