//go:build !darwin && !freebsd && !netbsd && !dragonfly && !openbsd && !windows

package pyfs

import "os"

// Sets mode 0700 on path, leaving a symbolic link untouched
func resetPerms(path string) error {
	if isSymlink(path) {
		return nil
	}
	return os.Chmod(path, 0o700)
}
