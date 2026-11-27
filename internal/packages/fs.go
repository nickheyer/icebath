package packages

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Removes a tree, refusing a link in its place
func removeTree(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&fs.ModeSymlink != 0 || junction(path) {
		return &fs.PathError{Op: "remove", Path: path, Err: errors.New("is a link, not a folder")}
	}
	return fsx.Retry(func() error { return os.RemoveAll(path) })
}

// Removes a tree, leaving a link in its place alone and every failure unsaid
func removeQuiet(path string) {
	if st, err := os.Lstat(path); err == nil && st.IsDir() && st.Mode()&fs.ModeSymlink == 0 && !junction(path) {
		_ = os.RemoveAll(path)
	}
}

// Path resolved through links as far as it exists, absolute
func resolved(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}
