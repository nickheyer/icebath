//go:build !windows

package profile

import (
	"math"
	"os"
	"syscall"
)

// User id that owns the path, false when it can't be told
func ownerOf(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

// Bytes a user may still store on the volume of the folder, negative when the volume tells nothing
func room(dir string) int64 {
	var stat syscall.Statfs_t
	if syscall.Statfs(dir, &stat) != nil || stat.Blocks == 0 {
		return -1
	}
	free := uint64(stat.Bavail) * uint64(stat.Bsize)
	return int64(min(free, math.MaxInt64))
}
