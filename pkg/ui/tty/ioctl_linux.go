//go:build linux || solaris || illumos || aix

package tty

import "golang.org/x/sys/unix"

// Termios ioctl requests of this platform
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
