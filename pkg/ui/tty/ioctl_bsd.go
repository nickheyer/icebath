//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package tty

import "golang.org/x/sys/unix"

// Termios ioctl requests of this platform
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
