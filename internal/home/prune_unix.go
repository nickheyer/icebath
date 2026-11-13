//go:build unix

package home

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/internal/proc"
)

// Line ending text files and the prune audit log are written with
const lineEnding = "\n"

// POSIX uids let a prune remember entries this user cannot delete
const stuckTrackingSupported = true

// Hard link count of a stat result, letting a subtree scan ignore files sharing an inode with another tree
func linkCount(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 0
}

// Reports whether a stat failure means the path is simply not a directory to follow
func ignorableStatError(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EBADF) || errors.Is(err, unix.ELOOP)
}

// Reports whether pid runs under the real uid of this process
func sameUser(pid int) bool {
	uid, err := proc.RealUID(pid)
	return err == nil && uid == os.Getuid()
}

// Inode, mtime and ctime of entry itself, false when it cannot be examined
func entryFingerprint(entry string) (fingerprint, bool) {
	var st unix.Stat_t
	if unix.Lstat(entry, &st) != nil {
		return fingerprint{}, false
	}
	return fingerprint{ino: uint64(st.Ino), mtime: st.Mtim.Nano(), ctime: st.Ctim.Nano()}, true
}

// Reports whether this user may now list and delete inside path by its effective ids, true once it is gone
func accessibleNow(path string) bool {
	if _, err := os.Lstat(path); err != nil {
		return true
	}
	return unix.Faccessat(unix.AT_FDCWD, path, unix.R_OK|unix.W_OK|unix.X_OK, unix.AT_EACCESS) == nil
}
