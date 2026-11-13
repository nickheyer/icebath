//go:build unix

package state

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Tightens a database and its sidecars to 0600 by path, optionally creating the main file, never opening an existing one
func (e *Engine) SecureDBFiles(path string, createMain bool) error {
	if createMain {
		dir, err := e.createDBFile(path)
		if err != nil || dir {
			return err
		}
	}
	for _, p := range sqliteFamily(path) {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			continue
		}
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, syscall.ENOENT) {
			return err
		}
	}
	return nil
}

// Creates a missing main file owner-only while no connection can open, true when the path is a directory
func (e *Engine) createDBFile(path string) (bool, error) {
	e.live.mu.Lock()
	defer e.live.mu.Unlock()
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	switch {
	case err == nil:
		if err := unix.Close(fd); err != nil {
			return false, &fs.PathError{Op: "close", Path: path, Err: err}
		}
	case errors.Is(err, unix.EEXIST):
	case errors.Is(err, unix.EISDIR):
		return true, nil
	default:
		return false, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return false, nil
}
