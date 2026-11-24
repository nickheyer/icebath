//go:build windows

package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Permission bits Windows attributes stand for: read and write unless read only, execute added for a .exe, .bat, .cmd or .com file; false when path cannot be read
func PermBits(path string) (uint32, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	mode := uint32(st.Mode().Perm())
	if !st.IsDir() {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".exe", ".bat", ".cmd", ".com":
			mode |= 0o111
		}
	}
	return mode, true
}

// Windows files have no POSIX owner to carry across a replace
func OwnerOf(string) *Owner { return nil }

func chown(string, int, int) {}

func chmodBits(path string, mode uint32) error { return os.Chmod(path, fs.FileMode(mode&0o777)) }
