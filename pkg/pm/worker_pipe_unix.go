//go:build !windows && !icebath_core && !icebath_noextras

package pm

import (
	"errors"
	"syscall"
)

// Whether a write failed because the child closed the reading end of its stdin
func brokenPipe(err error) bool { return errors.Is(err, syscall.EPIPE) }
