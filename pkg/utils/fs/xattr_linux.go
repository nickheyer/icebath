package fsutil

import (
	"bytes"
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

// Copies every extended attribute of src onto dst, symlinks followed, skipping names the filesystems do not support or deny
func copyXattr(src, dst string) error {
	return copyXattrWith(src, dst, unix.Listxattr, unix.Getxattr, unix.Setxattr)
}

// Copies every extended attribute of the symlink src itself onto the symlink dst, skipping names the filesystems do not support or deny
func copyLinkXattr(src, dst string) error {
	return copyXattrWith(src, dst, unix.Llistxattr, unix.Lgetxattr, unix.Lsetxattr)
}

func copyXattrWith(src, dst string,
	list func(string, []byte) (int, error),
	get func(string, string, []byte) (int, error),
	set func(string, string, []byte, int) error,
) error {
	names, err := xattrRead(func(buf []byte) (int, error) { return list(src, buf) })
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EINVAL) {
			return nil
		}
		return &fs.PathError{Op: "listxattr", Path: src, Err: err}
	}
	for name := range bytes.SplitSeq(bytes.TrimRight(names, "\x00"), []byte{0}) {
		if len(name) == 0 {
			continue
		}
		attr := string(name)
		value, err := xattrRead(func(buf []byte) (int, error) { return get(src, attr, buf) })
		if err == nil {
			err = set(dst, attr, value, 0)
		}
		if err != nil && !xattrCopyIgnored(err) {
			return &fs.PathError{Op: "setxattr", Path: dst, Err: err}
		}
	}
	return nil
}

// Bytes an attribute call fills, sized by a first call without a buffer and asked again when they grew in between
func xattrRead(read func([]byte) (int, error)) ([]byte, error) {
	for range 8 {
		size, err := read(nil)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return nil, nil
		}
		buf := make([]byte, size)
		n, err := read(buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
	return nil, unix.ERANGE
}

// Whether a failure reading or writing one attribute skips just that attribute
func xattrCopyIgnored(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENODATA) ||
		errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EACCES)
}
