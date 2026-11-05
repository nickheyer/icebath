package pyfs

import (
	"errors"
	"os"
	"path/filepath"
)

// Operation an rmtree error came from, re-runnable by the error handler like the func argument of onexc
type RmTreeOp func(path string) error

// Error rmtree raises for a symlinked root or a symlinked subdirectory that reports as a directory
var ErrRmTreeSymlink = errors.New("Cannot call rmtree on a symbolic link")

// Removes a tree like shutil.rmtree, routing every failure through onexc or returning the first one when onexc is nil
func RmTree(path string, onexc func(op RmTreeOp, path string, err error) error) error {
	handle := onexc
	if handle == nil {
		handle = func(_ RmTreeOp, _ string, err error) error { return err }
	}
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return handle(func(string) error { return nil }, path, &os.PathError{Op: "rmtree", Path: path, Err: ErrRmTreeSymlink})
	}
	return rmtreeUnsafe(path, handle)
}

// Depth first removal of one directory mirroring shutil._rmtree_unsafe
func rmtreeUnsafe(path string, onexc func(RmTreeOp, string, error) error) error {
	scandir := func(p string) error { _, err := os.ReadDir(p); return err }
	entries, err := os.ReadDir(path)
	if err != nil {
		if herr := onexc(scandir, path, err); herr != nil {
			return herr
		}
		entries = nil
	}
	for _, entry := range entries {
		full := filepath.Join(path, entry.Name())
		if entry.IsDir() {
			if err := rmtreeUnsafe(full, onexc); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(full); err != nil {
			if herr := onexc(unlinkOnly, full, err); herr != nil {
				return herr
			}
		}
	}
	if err := rmdirOnly(path); err != nil {
		return onexc(rmdirOnly, path, err)
	}
	return nil
}

// Removes a non directory entry the way os.unlink does
func unlinkOnly(p string) error {
	if st, err := os.Lstat(p); err == nil && st.IsDir() {
		return &os.PathError{Op: "unlink", Path: p, Err: errIsDir}
	}
	return os.Remove(p)
}

// Removes an empty directory the way os.rmdir does
func rmdirOnly(p string) error {
	if st, err := os.Lstat(p); err == nil && !st.IsDir() {
		return &os.PathError{Op: "rmdir", Path: p, Err: errNotDir}
	}
	return os.Remove(p)
}

var (
	errIsDir  = errors.New("Is a directory")
	errNotDir = errors.New("Not a directory")
)
