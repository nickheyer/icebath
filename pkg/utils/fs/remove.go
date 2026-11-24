package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
)

// Operation a tree removal failed at, which an error handler may run again on the same path once it fixed the cause
type RemoveOp func(path string) error

// Error RemoveTree reports for a root that is a symbolic link or a Windows junction, which it never follows
var ErrSymlinkRoot = errors.New("refusing to remove a tree through a symbolic link")

// Removes the tree at path depth first without following symbolic links or junctions, refusing a linked root with ErrSymlinkRoot; every failure goes to onError with the operation that failed, whose error stops the removal while nil goes on, and a nil onError stops at the first failure
func RemoveTree(path string, onError func(op RemoveOp, path string, err error) error) error {
	handle := onError
	if handle == nil {
		handle = func(_ RemoveOp, _ string, err error) error { return err }
	}
	if isSymlink(path) || isMountPoint(path) {
		return handle(func(string) error { return nil }, path, &fs.PathError{Op: "remove tree", Path: path, Err: ErrSymlinkRoot})
	}
	return removeTree(path, handle)
}

func removeTree(path string, onError func(RemoveOp, string, error) error) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if herr := onError(readDirOnly, path, err); herr != nil {
			return herr
		}
		entries = nil
	}
	for _, entry := range entries {
		full := filepath.Join(path, entry.Name())
		if entry.IsDir() {
			if err := removeTree(full, onError); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(full); err != nil {
			if herr := onError(unlinkOnly, full, err); herr != nil {
				return herr
			}
		}
	}
	if err := rmdirOnly(path); err != nil {
		return onError(rmdirOnly, path, err)
	}
	return nil
}

func readDirOnly(p string) error {
	_, err := os.ReadDir(p)
	return err
}

// Removes an entry that is not a directory
func unlinkOnly(p string) error {
	if st, err := os.Lstat(p); err == nil && st.IsDir() {
		return &fs.PathError{Op: "unlink", Path: p, Err: syscall.EISDIR}
	}
	return os.Remove(p)
}

// Removes an empty directory
func rmdirOnly(p string) error {
	if st, err := os.Lstat(p); err == nil && !st.IsDir() {
		return &fs.PathError{Op: "rmdir", Path: p, Err: syscall.ENOTDIR}
	}
	return os.Remove(p)
}

// Removes the regular files directly in dir whose names match the glob pattern and that were last modified before cutoff, symlinks followed to decide; gives back how many went, skipping whatever cannot be read or removed
func RemoveOlderThan(dir, pattern string, cutoff time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !fnmatch.Fnmatch(e.Name(), pattern) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || !st.ModTime().Before(cutoff) {
			continue
		}
		if os.Remove(p) == nil {
			removed++
		}
	}
	return removed
}
