package fsutil

import (
	"errors"
	"io/fs"
	"path/filepath"
	"syscall"
)

// Removes a temporary directory tree, giving back owner access to every entry or parent that refuses removal and trying it again; entries already gone are no failure
func RemoveTempDir(dir string) error { return removeTemp(dir, false) }

// Removes a temporary directory when the function that made it returns, joining a removal failure into *errp; meant for defer
func CleanupTempDir(dir string, errp *error) {
	if err := RemoveTempDir(dir); err != nil {
		*errp = errors.Join(*errp, err)
	}
}

// RemoveTree of name whose handler retries a denied entry after resetting the permissions of it and its parent
func removeTemp(name string, repeated bool) error {
	return RemoveTree(name, func(_ RemoveOp, path string, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, fs.ErrPermission) && !errors.Is(err, syscall.EISDIR) {
			return err
		}
		if repeated && path == name {
			return err
		}
		if err := resetAndRemove(name, path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}

// Resets the permissions of path and of its parent, then removes it, a directory through another removeTemp
func resetAndRemove(name, path string) error {
	if path != name {
		if err := resetPerms(filepath.Dir(path)); err != nil {
			return err
		}
	}
	if err := resetPerms(path); err != nil {
		return err
	}
	err := unlinkOnly(path)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EISDIR):
		return removeTemp(path, path == name)
	case errors.Is(err, fs.ErrPermission):
		if !isDir(path) || isMountPoint(path) {
			return err
		}
		return removeTemp(path, path == name)
	}
	return err
}
