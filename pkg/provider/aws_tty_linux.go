//go:build linux

package provider

import "golang.org/x/sys/unix"

// Termios ioctls, the set variant flushing pending input like TCSAFLUSH
const (
	awsIoctlGetTermios      = unix.TCGETS
	awsIoctlSetTermiosFlush = unix.TCSETSF
)
