//go:build !windows

package fsutil

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// Block counts of a volume in units of unit bytes: every block, the free ones, and those an unprivileged user may still take
type volumeBlocks struct{ blocks, free, avail, unit uint64 }

// Total, used and free bytes of the volume holding path, free counting what an unprivileged user may still use
func DiskUsage(path string) (total, used, free uint64, err error) {
	v, err := volumeOf(path)
	if err != nil {
		return 0, 0, 0, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	return v.blocks * v.unit, (v.blocks - min(v.free, v.blocks)) * v.unit, v.avail * v.unit, nil
}

// Whether the real user may use path with every one of the AccessRead, AccessWrite and AccessExec bits of mode
func Access(path string, mode uint32) bool { return unix.Access(path, mode) == nil }
