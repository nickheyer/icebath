package pyfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Removes a temporary directory like tempfile.TemporaryDirectory cleanup, resetting permissions of denied entries
func RemoveTempDir(name string) error { return tempRmtree(name, false) }

// Cleans up a temporary directory at the end of its with block, a cleanup error replacing the block result
func CloseTempDir(name string, errp *error) {
	if err := RemoveTempDir(name); err != nil {
		*errp = err
	}
}

// shutil.rmtree of name whose handler retries denied entries after resetting their flags and modes
func tempRmtree(name string, repeated bool) error {
	return RmTree(name, func(_ RmTreeOp, path string, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, fs.ErrPermission) && !isDirError(err) {
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

// Resets the permissions of path and its parent then unlinks it, recursing into a directory
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
	case isDirError(err):
		return tempRmtree(path, path == name)
	case errors.Is(err, fs.ErrPermission):
		if st, serr := os.Stat(path); serr != nil || !st.IsDir() || isMountPoint(path) {
			return err
		}
		return tempRmtree(path, path == name)
	}
	return err
}

// Whether err is the IsADirectoryError of an unlink on a directory
func isDirError(err error) bool {
	return errors.Is(err, errIsDir) || errors.Is(err, syscall.EISDIR)
}

// Whether path is a symbolic link, false when it cannot be read
func isSymlink(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}
