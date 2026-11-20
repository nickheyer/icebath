//go:build !windows

package state

import (
	"errors"
	"os"
	"syscall"
)

// Creates or tightens a writable state database and its wal and shm sidecars to 0600, symlinks untouched
func secureStateDBFiles(dbPath string, createMain bool) error {
	if createMain {
		fd, err := syscall.Open(dbPath, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
		switch {
		case err == nil:
			syscall.Close(fd)
		case errors.Is(err, syscall.EEXIST):
		case errors.Is(err, syscall.EISDIR):
			return nil
		default:
			return &os.PathError{Op: "open", Path: dbPath, Err: err}
		}
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		st, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			continue
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	return nil
}
