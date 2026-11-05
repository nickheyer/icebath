//go:build !windows

package tools

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Device and inode of the path with symlinks followed
func checkpointDevIno(path string) (uint64, uint64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("stat result carries no device identity")
	}
	return uint64(sys.Dev), uint64(sys.Ino), nil
}

// Reports a mount point the way posixpath.ismount does, a device change or an inode shared with the parent
func checkpointIsMount(path string) bool {
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeSymlink != 0 {
		return false
	}
	s1, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	parent := resolveNonStrict(filepath.Join(path, ".."))
	pst, err := os.Lstat(parent)
	if err != nil {
		return false
	}
	s2, ok := pst.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return s1.Dev != s2.Dev || s1.Ino == s2.Ino
}
