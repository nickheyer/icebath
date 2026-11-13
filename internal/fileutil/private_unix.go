//go:build unix

package fileutil

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// Leaf directory at mode 0700 whatever the umask, an existing one chmodded down to it
func mkdirPrivate(path string) error {
	err := os.Mkdir(path, 0o700)
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		fi, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		if !fi.IsDir() {
			return &fs.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
		}
	}
	return os.Chmod(path, 0o700)
}
