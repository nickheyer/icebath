//go:build !windows

package pypath

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func errnoName(errno syscall.Errno) string { return unix.ErrnoName(errno) }
