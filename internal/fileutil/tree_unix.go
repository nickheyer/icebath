//go:build unix

package fileutil

import (
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Copies the bytes of src into dst and then its metadata
func copyFile(src, dst string) error {
	if err := copyData(src, dst); err != nil {
		return err
	}
	return copyMeta(src, dst, true)
}

// Copies access and modification times, extended attributes, permission bits and file flags of src onto dst
func copyMeta(src, dst string, follow bool) error {
	var st unix.Stat_t
	var err error
	flags := 0
	if follow {
		err = unix.Stat(src, &st)
	} else {
		err = unix.Lstat(src, &st)
		flags = unix.AT_SYMLINK_NOFOLLOW
	}
	if err != nil {
		return &fs.PathError{Op: "stat", Path: src, Err: err}
	}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, dst, []unix.Timespec{st.Atim, st.Mtim}, flags); err != nil {
		return &fs.PathError{Op: "utimes", Path: dst, Err: err}
	}
	if err := copyXattrs(src, dst, follow); err != nil {
		return err
	}
	if follow || lchmodSupported {
		if err := unix.Fchmodat(unix.AT_FDCWD, dst, uint32(st.Mode)&0o7777, flags); err != nil {
			return &fs.PathError{Op: "chmod", Path: dst, Err: err}
		}
	}
	return copyFlags(dst, &st, follow)
}

// Lifts what blocks deleting p, clearing file flags and adding owner rwx, a symlink left untouched
func unblock(p string) {
	var st unix.Stat_t
	if unix.Lstat(p, &st) != nil || uint32(st.Mode)&unix.S_IFMT == unix.S_IFLNK {
		return
	}
	clearFlags(p, &st)
	_ = unix.Chmod(p, uint32(st.Mode)&0o7777|0o700)
}

// Reports whether a directory entry is a real directory to descend into
func realDir(e fs.DirEntry) bool { return e.IsDir() }

// Sharing violations exist only on Windows
func sharingViolation(error) bool { return false }

// Reports whether name matches the glob pattern, case sensitively as POSIX filesystems compare names
func matchName(pattern, name string) (bool, error) { return filepath.Match(pattern, name) }

// POSIX tree copies report a directory whose times or mode could not be copied
const dirMetaReported = true

// Renames src to dst as rename does, replacing a file already at dst
func renameNoReplace(src, dst string) error { return os.Rename(src, dst) }
