//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package pyshutil

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Copies nothing because shutil copies extended attributes only where os.listxattr exists
func copyLinkXattr(_, _ string) error { return nil }

// Sets the mode and the file flags of a symlink itself, an unsupported operation skipped like CPython does
func copyLinkModeAndFlags(fi os.FileInfo, dst string) error {
	mode := uint32(fi.Mode().Perm())
	if fi.Mode()&os.ModeSetuid != 0 {
		mode |= unix.S_ISUID
	}
	if fi.Mode()&os.ModeSetgid != 0 {
		mode |= unix.S_ISGID
	}
	if fi.Mode()&os.ModeSticky != 0 {
		mode |= unix.S_ISVTX
	}
	unsupported := func(err error) bool { return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) }
	if err := unix.Fchmodat(unix.AT_FDCWD, dst, mode, unix.AT_SYMLINK_NOFOLLOW); err != nil && !unsupported(err) {
		return &os.PathError{Op: "chmod", Path: dst, Err: err}
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
		return &os.PathError{Op: "chflags", Path: dst, Err: err}
	}
	return nil
}
