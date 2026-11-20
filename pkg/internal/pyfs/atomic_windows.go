//go:build windows

package pyfs

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// Windows synthesizes mode bits so a new file gets no explicit mode
func defaultNewFileMode() (os.FileMode, bool) { return 0, false }

// Text mode writes translate LF to CRLF on Windows
func PlatformNewlines(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// Access denied, sharing and lock violations a held target reports on rename
func IsContendedReplaceError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == 5 || errno == 32 || errno == 33
}

// Not same device and busy target renames fall back to a copy, as Python maps them to EXDEV and EBUSY
func IsCrossDeviceError(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) && (errno == 17 || errno == 170) {
		return true
	}
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EBUSY)
}

// Toggles the read only attribute of the open staged file from the owner write bit like os.fchmod on Windows
func fchmodStaged(f *os.File, mode uint32) error {
	return f.Chmod(os.FileMode(mode & 0o777))
}
