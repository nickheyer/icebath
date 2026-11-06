package pyshutil

import (
	"errors"
	"strings"

	"golang.org/x/sys/unix"
)

// Copies every extended attribute the way shutil._copyxattr does, skipping unsupported and denied names
func copyXattr(src, dst string) error {
	ignored := func(err error) bool {
		return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EINVAL)
	}
	size, err := unix.Listxattr(src, nil)
	if err != nil {
		if ignored(err) {
			return nil
		}
		return err
	}
	buf := make([]byte, size)
	n, err := unix.Listxattr(src, buf)
	if err != nil {
		if ignored(err) {
			return nil
		}
		return err
	}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:n]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		vsize, err := unix.Getxattr(src, name, nil)
		if err == nil {
			value := make([]byte, vsize)
			var vn int
			if vn, err = unix.Getxattr(src, name, value); err == nil {
				err = unix.Setxattr(dst, name, value[:vn], 0)
			}
		}
		if err != nil && !errors.Is(err, unix.EPERM) && !ignored(err) {
			return err
		}
	}
	return nil
}
