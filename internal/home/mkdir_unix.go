//go:build unix

package home

import (
	"io/fs"
	"os"
	"syscall"
)

// Creates the single directory path with perm filtered by the umask
func mkdirLeaf(path string, perm fs.FileMode) error { return os.Mkdir(path, perm) }

// Installs mask as the process umask and returns the one it replaced
func setUmask(mask int) int { return syscall.Umask(mask) }

// Puts back a umask setUmask replaced
func restoreUmask(previous int) { syscall.Umask(previous) }
