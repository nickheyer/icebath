//go:build darwin || freebsd

package pm

import (
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Last access time the inode records
func fileAccessTime(path string, st os.FileInfo) (time.Time, error) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, &os.PathError{Op: "stat", Path: path, Err: syscall.EINVAL}
	}
	return time.Unix(int64(sys.Atimespec.Sec), int64(sys.Atimespec.Nsec)), nil
}

// Python exposes no xattr calls on this platform, so copystat copies none
func copyXattrs(src, dst string) error { return nil }

// Copies the BSD file flags, tolerating a filesystem that does not support them
func copyFlags(dst string, st os.FileInfo) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := unix.Chflags(dst, int(sys.Flags)); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) {
			return nil
		}
		return &os.PathError{Op: "chflags", Path: dst, Err: err}
	}
	return nil
}
