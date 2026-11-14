//go:build !windows

package selfupdate

import (
	"errors"
	"io/fs"
	"syscall"
)

// True for the EPERM, EACCES and EBUSY failures a held file produces and that clear by waiting
func frontendHeld(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EBUSY)
}

// True when a removal met a directory that is not empty
func frontendNotEmpty(err error) bool { return errors.Is(err, syscall.ENOTEMPTY) }

// True when the process or system ran out of file descriptors
func frontendTooManyFiles(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}
