//go:build !windows

package checkpoints

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Device and inode of the path, links followed
func identity(path string) (uint64, uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("stat result carries no device identity")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

// Whether a failed stat says the path is not there, any other failure being no evidence of that
func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ELOOP)
}

// Whether the folder is a mount point: on another device than its parent, or the same inode as it
func mount(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	parent, err := os.Lstat(filepath.Join(path, ".."))
	if err != nil {
		return false
	}
	a, ok := info.Sys().(*syscall.Stat_t)
	b, ok2 := parent.Sys().(*syscall.Stat_t)
	return ok && ok2 && (a.Dev != b.Dev || a.Ino == b.Ino)
}
