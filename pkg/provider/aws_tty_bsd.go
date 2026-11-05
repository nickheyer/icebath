//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package provider

import "golang.org/x/sys/unix"

// Termios ioctls, the set variant flushing pending input like TCSAFLUSH
const (
	awsIoctlGetTermios      = unix.TIOCGETA
	awsIoctlSetTermiosFlush = unix.TIOCSETAF
)
