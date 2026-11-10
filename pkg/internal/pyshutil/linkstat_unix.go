//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pyshutil

import (
	"os"

	"golang.org/x/sys/unix"
)

// Copies the stat of one symlink onto another without following either, shutil.copystat with follow_symlinks false
func CopyLinkStat(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	times := []unix.Timespec{unix.NsecToTimespec(fileAtime(fi).UnixNano()), unix.NsecToTimespec(fi.ModTime().UnixNano())}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, dst, times, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "utime", Path: dst, Err: err}
	}
	if err := copyLinkXattr(src, dst); err != nil {
		return err
	}
	return copyLinkModeAndFlags(fi, dst)
}
