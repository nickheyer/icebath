//go:build darwin

package pyfs

import (
	"os"

	"golang.org/x/sys/unix"
)

// Clears the file flags and sets mode 0700 on path itself without following a link
func resetPerms(path string) error {
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_FLAGS}
	if err := unix.Setattrlist(path, &attrs, make([]byte, 4), unix.FSOPT_NOFOLLOW); err != nil {
		return &os.PathError{Op: "lchflags", Path: path, Err: err}
	}
	if err := unix.Fchmodat(unix.AT_FDCWD, path, 0o700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &os.PathError{Op: "lchmod", Path: path, Err: err}
	}
	return nil
}
