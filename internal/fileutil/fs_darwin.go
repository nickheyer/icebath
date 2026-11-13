package fileutil

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// macOS has lchmod, so a copied symlink takes the permission bits of its source
const lchmodSupported = true

// User and system immutable flags whose presence alone refuses a directory move
const (
	userImmutable   = 0x2
	systemImmutable = 0x20000
)

// macOS keeps extended attributes out of a copy
func copyXattrs(string, string, bool) error { return nil }

// Copies the BSD file flags of a source onto dst, a filesystem without flags accepting none
func copyFlags(dst string, st *unix.Stat_t, follow bool) error {
	var err error
	if follow {
		err = unix.Chflags(dst, int(st.Flags))
	} else {
		err = setFlagsNoFollow(dst, st.Flags)
	}
	if err != nil && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
		return &fs.PathError{Op: "chflags", Path: dst, Err: err}
	}
	return nil
}

// Sets the file flags of path itself without following a final symlink
func setFlagsNoFollow(path string, flags uint32) error {
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_FLAGS}
	buf := make([]byte, 4)
	binary.NativeEndian.PutUint32(buf, flags)
	return unix.Setattrlist(path, &attrs, buf, unix.FSOPT_NOFOLLOW)
}

// Clears any file flags on p so an immutable or append only entry can be deleted
func clearFlags(p string, st *unix.Stat_t) {
	if st.Flags != 0 {
		_ = setFlagsNoFollow(p, 0)
	}
}

// Reports whether a directory move is refused before any copy, the tree immutable or unwritable while holding entries
func moveRefused(src string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Stat(src, &st); err != nil {
		return false, &fs.PathError{Op: "stat", Path: src, Err: err}
	}
	if st.Flags == userImmutable || st.Flags == systemImmutable {
		return true, nil
	}
	if unix.Access(src, unix.W_OK) == nil {
		return false, nil
	}
	entries, err := os.ReadDir(src)
	return len(entries) > 0, err
}

// Fragment size statvfs reports on macOS, the fundamental block size
func fragmentSize(st *unix.Statfs_t) uint64 { return uint64(st.Bsize) }
