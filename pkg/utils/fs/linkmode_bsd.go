//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Sets the mode and the file flags of the symlink dst itself from fi, skipping what the filesystem does not support
func copyLinkModeAndFlags(fi fs.FileInfo, dst string) error {
	unsupported := func(err error) bool { return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) }
	if err := unix.Fchmodat(unix.AT_FDCWD, dst, posixBits(fi.Mode()), unix.AT_SYMLINK_NOFOLLOW); err != nil && !unsupported(err) {
		return &fs.PathError{Op: "chmod", Path: dst, Err: err}
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if copied, err := os.Lstat(dst); err == nil {
		if have, ok := copied.Sys().(*syscall.Stat_t); ok && have.Flags == st.Flags {
			return nil
		}
	}
	if err := lchflags(dst, st.Flags); err != nil && !unsupported(err) {
		return &fs.PathError{Op: "chflags", Path: dst, Err: err}
	}
	return nil
}
