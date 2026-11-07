//go:build !windows

package backup

import (
	"os"
	"syscall"
)

// Creates or tightens a file to owner only mode before SQLite writes it, refusing a planted symlink
func secureCreate(dst string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(0o600)
}
