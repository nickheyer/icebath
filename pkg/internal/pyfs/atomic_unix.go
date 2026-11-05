//go:build !windows

package pyfs

import (
	"errors"
	"os"
	"syscall"
)

const canFchmod = true

// Mode a plain create gives a new file under the process umask
func defaultNewFileMode() (os.FileMode, bool) {
	current := syscall.Umask(0o077)
	syscall.Umask(current)
	return os.FileMode(0o666 &^ current), true
}

// Text mode writes keep LF on POSIX hosts
func platformNewlines(s string) string { return s }

// Contended rename retries only exist on Windows
func isContendedReplaceError(err error) bool { return false }

// Cross device and busy target renames fall back to a copy
func isCrossDeviceError(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EBUSY)
}

// Owning uid and gid of an existing file
type fileOwner struct{ uid, gid int }

// Owner of path on POSIX hosts, nil when it cannot be read
func ownerOf(path string) *fileOwner {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return &fileOwner{uid: int(sys.Uid), gid: int(sys.Gid)}
}
