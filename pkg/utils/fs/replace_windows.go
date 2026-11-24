//go:build windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows derives permission bits from attributes, so a new file is given none
func newFileMode() (uint32, bool) { return 0, false }

// Whether a rename failed with the access denied, sharing or lock violation a target another process holds open reports
func IsRenameContended(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == windows.ERROR_ACCESS_DENIED || errno == windows.ERROR_SHARING_VIOLATION || errno == windows.ERROR_LOCK_VIOLATION
}

// Whether a rename failed because the target is on another volume or busy, a failure a copy works around
func IsRenameCrossDevice(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) && (errno == windows.ERROR_NOT_SAME_DEVICE || errno == windows.ERROR_BUSY) {
		return true
	}
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EBUSY)
}

// Sets the read only attribute of the open temp file from the owner write bit before it is published
func fchmodStaged(f *os.File, mode uint32) error { return f.Chmod(fs.FileMode(mode & 0o777)) }
