//go:build !windows

package home

import (
	"io/fs"
	"os"
	"syscall"
)

func probeProcessElevated() bool { return false }

// The native Windows ACL writer, absent here like a failed pywin32 import
func grantNativeACL(string, bool, string) (bool, error) { return false, errNoNativeACL }

func lstatForACL(path string) (aclStat, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return aclStat{}, err
	}
	st := aclStat{link: fi.Mode()&fs.ModeSymlink != 0, dir: fi.IsDir(), nlink: linkCount(fi)}
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.dev, st.ino = uint64(s.Dev), uint64(s.Ino)
	}
	return st, nil
}

func osMkdir(p string, mode os.FileMode) error { return os.Mkdir(p, mode) }
