package home

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Home subdirectory, a populated legacy directory winning over the new subpath
func HermesDir(h, newSubpath, oldName string) string {
	old := filepath.Join(h, oldName)
	if legacyPathHasContent(old) {
		return old
	}
	return filepath.Join(h, newSubpath)
}

// True for a non directory file or a populated directory, an unreadable path counting as occupied
func legacyPathHasContent(p string) bool {
	st, err := os.Lstat(p)
	if err == nil && st.Mode()&os.ModeSymlink != 0 {
		st, err = os.Stat(p)
	}
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	if !st.IsDir() {
		return true
	}
	f, err := os.Open(p)
	if err != nil {
		return true
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return len(names) > 0 || (err != nil && !errors.Is(err, io.EOF))
}
