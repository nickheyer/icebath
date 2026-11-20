package pyshutil

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Copies every extended attribute of a symlink itself, skipping unsupported and denied names like shutil._copyxattr
func copyLinkXattr(src, dst string) error {
	listIgnored := func(err error) bool {
		return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EINVAL)
	}
	size, err := unix.Llistxattr(src, nil)
	if err != nil {
		if listIgnored(err) {
			return nil
		}
		return &os.PathError{Op: "listxattr", Path: src, Err: err}
	}
	buf := make([]byte, size)
	n, err := unix.Llistxattr(src, buf)
	if err != nil {
		if listIgnored(err) {
			return nil
		}
		return &os.PathError{Op: "listxattr", Path: src, Err: err}
	}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:n]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		vsize, err := unix.Lgetxattr(src, name, nil)
		if err == nil {
			value := make([]byte, vsize)
			var vn int
			if vn, err = unix.Lgetxattr(src, name, value); err == nil {
				err = unix.Lsetxattr(dst, name, value[:vn], 0)
			}
		}
		if err != nil && !xattrCopyIgnored(err) {
			return &os.PathError{Op: "setxattr", Path: dst, Err: err}
		}
	}
	return nil
}

// Whether shutil._copyxattr skips an attribute after this failure reading or writing it
func xattrCopyIgnored(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENODATA) ||
		errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EACCES)
}

// Leaves the mode alone because Linux cannot chmod a symlink, the failure CPython suppresses
func copyLinkModeAndFlags(_ os.FileInfo, _ string) error { return nil }
