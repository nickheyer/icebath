//go:build !windows

package gateway

import (
	"os"
	"syscall"
)

// Opens a blob read only without following a final symlink
func openBlobNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
