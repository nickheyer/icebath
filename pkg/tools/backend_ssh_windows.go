package tools

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Reports the Windows privilege error raised when symlink creation needs Developer Mode
func symlinkPrivilegeMissing(err error) bool {
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}
