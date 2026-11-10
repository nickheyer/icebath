//go:build windows

package pyfs

import (
	"os"
	"strings"
)

// Mode bits Python synthesizes for the file path names, false when it is missing or cannot be read
func PreserveFileMode(path string) (uint32, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	mode := uint32(st.Mode().Perm())
	if !st.IsDir() {
		switch strings.ToLower(path[max(strings.LastIndex(path, "."), 0):]) {
		case ".exe", ".bat", ".cmd", ".com":
			mode |= 0o111
		}
	}
	return mode, true
}

// Windows files have no POSIX owner to carry across a replace
func PreserveFileOwner(string) *FileOwner { return nil }

func chownPath(string, int, int) {}

func chmodPath(path string, mode uint32) { _ = os.Chmod(path, os.FileMode(mode&0o777)) }
