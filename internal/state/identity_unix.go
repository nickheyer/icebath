//go:build unix

package state

import (
	"io/fs"
	"os"
	"syscall"
)

// Device and inode stat reports for path, symlinks followed
func identityOf(path string) (fileIdentity, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileIdentity{}, err
	}
	return infoIdentity(path, fi)
}

// Device and inode recorded in a stat result
func infoIdentity(path string, fi fs.FileInfo) (fileIdentity, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, &fs.PathError{Op: "stat", Path: path, Err: syscall.EINVAL}
	}
	return fileIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}
