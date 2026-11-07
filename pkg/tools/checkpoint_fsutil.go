package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Deletes a tree that plain rmtree refuses, read only files and directories included
func RmtreeReadonly(path string, ignoreErrors bool) error { return rmtreeReadonly(path, ignoreErrors) }

// Deletes a tree, clearing the write bit on a failing path and its parent before retrying that one operation
func rmtreeReadonly(path string, ignoreErrors bool) error {
	err := rmtreeTop(path)
	if err != nil && !ignoreErrors {
		return err
	}
	return nil
}

// Refuses a symlink root the way shutil.rmtree does, then removes the tree
func rmtreeTop(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return &os.PathError{Op: "rmtree", Path: path, Err: errors.New("Cannot call rmtree on a symbolic link")}
	}
	return rmtreeDir(path)
}

// Removes a directory's entries depth first and then the directory itself
func rmtreeDir(dir string) error {
	var names []string
	err := rmtreeRetry(dir, func() error {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		names, err = f.Readdirnames(-1)
		return err
	})
	if err != nil {
		return err
	}
	for _, name := range names {
		p := filepath.Join(dir, name)
		st, err := os.Lstat(p)
		if err == nil && st.IsDir() {
			if err := rmtreeDir(p); err != nil {
				return err
			}
			continue
		}
		if err := rmtreeRetry(p, func() error { return rmtreeRemove(p) }); err != nil {
			return err
		}
	}
	return rmtreeRetry(dir, func() error { return rmtreeRemove(dir) })
}

// Single entry removal inside a tree delete, swapped by tests
var rmtreeRemove = os.Remove

// Retries an operation once after granting owner write and search on the path and its parent, permission errors only
func rmtreeRetry(p string, op func() error) error {
	err := op()
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	for _, candidate := range []string{filepath.Dir(p), p} {
		if st, serr := os.Stat(candidate); serr == nil {
			keep := st.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
			os.Chmod(candidate, keep|0o300)
		}
	}
	return op()
}

// Reads a text marker as UTF-8 with an optional byte order mark, an error for undecodable bytes
func readUTF8SigText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", &checkpointValueError{msg: "'utf-8' codec can't decode " + path}
	}
	return strings.TrimPrefix(string(raw), "\ufeff"), nil
}
