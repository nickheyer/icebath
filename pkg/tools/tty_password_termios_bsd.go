//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package tools

import "golang.org/x/sys/unix"

// Termios ioctls, the set variant flushing pending input like TCSAFLUSH
const (
	ioctlGetTermios      = unix.TIOCGETA
	ioctlSetTermiosFlush = unix.TIOCSETAF
)
