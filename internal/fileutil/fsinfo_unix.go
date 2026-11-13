//go:build unix

package fileutil

import (
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Total, used and free bytes of the volume holding path, free counting what an unprivileged user may still write
func DiskUsage(path string) (DiskSpace, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return DiskSpace{}, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	frag := fragmentSize(&st)
	return DiskSpace{
		Total: uint64(st.Blocks) * frag,
		Used:  (uint64(st.Blocks) - uint64(st.Bfree)) * frag,
		Free:  uint64(st.Bavail) * frag,
	}, nil
}

// Reports whether the real user may use path with every permission in a, a missing path allowing nothing
func Accessible(path string, a Access) bool {
	return unix.Access(path, uint32(a)) == nil
}

// Device and inode of path itself, a final symlink not followed
func LstatID(path string) (FileID, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return FileID{}, err
	}
	id, _, err := linkIdentity(path, fi)
	return id, err
}

// Device, inode and hard link count recorded in a stat result
func linkIdentity(path string, fi fs.FileInfo) (FileID, uint64, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileID{}, 0, &fs.PathError{Op: "stat", Path: path, Err: syscall.EINVAL}
	}
	return FileID{Device: uint64(st.Dev), Inode: st.Ino}, uint64(st.Nlink), nil
}
