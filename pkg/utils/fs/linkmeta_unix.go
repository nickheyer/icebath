//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package fsutil

import (
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// Copies the times, on Linux the extended attributes and on the BSDs and macOS the mode and file flags of the symlink src onto the symlink dst, following neither
func CopyLinkMetadata(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	times := []unix.Timespec{unix.NsecToTimespec(Atime(fi).UnixNano()), unix.NsecToTimespec(fi.ModTime().UnixNano())}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, dst, times, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "utimes", Path: dst, Err: err}
	}
	if err := copyLinkXattr(src, dst); err != nil {
		return err
	}
	return copyLinkModeAndFlags(fi, dst)
}
