package fileutil

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Linux has no lchmod, so a copied symlink keeps the permission bits it was created with
const lchmodSupported = false

// Copies every extended attribute of src onto dst, skipping ones the filesystems cannot carry or this user cannot set
func copyXattrs(src, dst string, follow bool) error {
	names, err := xattrNames(src, follow)
	if err != nil {
		if xattrSkippable(err) {
			return nil
		}
		return &fs.PathError{Op: "listxattr", Path: src, Err: err}
	}
	for _, name := range names {
		value, err := xattrValue(src, name, follow)
		if err == nil {
			if follow {
				err = unix.Setxattr(dst, name, value, 0)
			} else {
				err = unix.Lsetxattr(dst, name, value, 0)
			}
		}
		if err != nil && !xattrSkippable(err) && !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EACCES) {
			return &fs.PathError{Op: "setxattr", Path: dst, Err: err}
		}
	}
	return nil
}

// Reports whether an attribute call failed because the filesystem or the attribute does not support it
func xattrSkippable(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EINVAL)
}

// Names of the extended attributes on path
func xattrNames(path string, follow bool) ([]string, error) {
	list := func(dest []byte) (int, error) {
		if follow {
			return unix.Listxattr(path, dest)
		}
		return unix.Llistxattr(path, dest)
	}
	for {
		n, err := list(nil)
		if err != nil || n == 0 {
			return nil, err
		}
		buf := make([]byte, n)
		n, err = list(buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var names []string
		for _, name := range strings.Split(string(buf[:n]), "\x00") {
			if name != "" {
				names = append(names, name)
			}
		}
		return names, nil
	}
}

// Value of one extended attribute on path
func xattrValue(path, name string, follow bool) ([]byte, error) {
	get := func(dest []byte) (int, error) {
		if follow {
			return unix.Getxattr(path, name, dest)
		}
		return unix.Lgetxattr(path, name, dest)
	}
	for {
		n, err := get(nil)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, n)
		n, err = get(buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}

// Linux files carry no BSD file flags to copy
func copyFlags(string, *unix.Stat_t, bool) error { return nil }

// Linux files carry no BSD file flags to clear
func clearFlags(string, *unix.Stat_t) {}

// Stats the source as a directory move does, Linux stat results carrying no immutable flag to refuse on
func moveRefused(src string) (bool, error) {
	_, err := os.Stat(src)
	return false, err
}

// Fragment size statfs reports, the block size when the kernel leaves it zero
func fragmentSize(st *unix.Statfs_t) uint64 {
	if st.Frsize != 0 {
		return uint64(st.Frsize)
	}
	return uint64(st.Bsize)
}
