package pm

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Last access time the inode records
func fileAccessTime(path string, st os.FileInfo) (time.Time, error) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, &os.PathError{Op: "stat", Path: path, Err: syscall.EINVAL}
	}
	return time.Unix(int64(sys.Atim.Sec), int64(sys.Atim.Nsec)), nil
}

// Copies every extended attribute, skipping the ones the destination filesystem refuses like shutil does
func copyXattrs(src, dst string) error {
	size, err := unix.Listxattr(src, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EINVAL) {
			return nil
		}
		return &os.PathError{Op: "listxattr", Path: src, Err: err}
	}
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	size, err = unix.Listxattr(src, buf)
	if err != nil {
		return &os.PathError{Op: "listxattr", Path: src, Err: err}
	}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:size]), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		if err := copyXattr(src, dst, name); err != nil {
			if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENODATA) ||
				errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EACCES) {
				continue
			}
			return &os.PathError{Op: "setxattr", Path: dst, Err: err}
		}
	}
	return nil
}

// Copies one extended attribute value
func copyXattr(src, dst, name string) error {
	size, err := unix.Getxattr(src, name, nil)
	if err != nil {
		return err
	}
	value := make([]byte, size)
	if size > 0 {
		if size, err = unix.Getxattr(src, name, value); err != nil {
			return err
		}
	}
	return unix.Setxattr(dst, name, value[:size], 0)
}

// Linux files carry no BSD flags for copystat to copy
func copyFlags(dst string, st os.FileInfo) error { return nil }
