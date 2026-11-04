//go:build linux

package tools

import "golang.org/x/sys/unix"

// Termios ioctls, the set variant flushing pending input like TCSAFLUSH
const (
	ioctlGetTermios      = unix.TCGETS
	ioctlSetTermiosFlush = unix.TCSETSF
)
