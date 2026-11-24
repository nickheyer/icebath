//go:build !windows

package fsutil

import (
	"errors"

	"golang.org/x/sys/unix"
)

// Whether no process with the pid exists, one this process may not signal still counting as alive
func processGone(pid int) bool { return errors.Is(unix.Kill(pid, 0), unix.ESRCH) }
