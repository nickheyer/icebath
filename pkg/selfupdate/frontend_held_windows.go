//go:build windows

package selfupdate

import (
	"errors"
	"io/fs"
	"syscall"
)

// True for access denied, sharing and lock violations, the codes Node reports as EPERM, EACCES and EBUSY
func frontendHeld(err error) bool {
	const sharingViolation, lockViolation = syscall.Errno(32), syscall.Errno(33)
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, sharingViolation) || errors.Is(err, lockViolation)
}

// True for the directory not empty code Node reports as ENOTEMPTY
func frontendNotEmpty(err error) bool { return errors.Is(err, syscall.Errno(145)) }

// True for the too many open files code Node reports as EMFILE
func frontendTooManyFiles(err error) bool { return errors.Is(err, syscall.Errno(4)) }
