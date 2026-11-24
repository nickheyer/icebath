//go:build !windows

package fsutil

import (
	"io/fs"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// POSIX hosts run files by their mode, not by their extension
func executableExts() []string { return nil }

func hasPathSeparator(cmd string) bool { return strings.Contains(cmd, "/") }

// The file itself when it is no directory and the real user may execute it
func findExecutable(file string, _ []string) (string, error) {
	st, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", syscall.EISDIR
	}
	if err := unix.Access(file, unix.X_OK); err != nil {
		return "", fs.ErrPermission
	}
	return file, nil
}
