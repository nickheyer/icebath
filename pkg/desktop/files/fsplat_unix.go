//go:build !windows

package files

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Canonical absolute path with every link resolved, the realpath(3) answer
func realpath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// True when the process may read the file
func readable(p string) bool { return unix.Access(p, unix.R_OK) == nil }
