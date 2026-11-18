//go:build windows && !icebath_core && !icebath_noextras

package pm

import (
	"errors"
	"syscall"
)

// Whether a write failed because the child closed the reading end of its stdin, ERROR_NO_DATA being 232
func brokenPipe(err error) bool {
	return errors.Is(err, syscall.ERROR_BROKEN_PIPE) || errors.Is(err, syscall.Errno(232))
}
