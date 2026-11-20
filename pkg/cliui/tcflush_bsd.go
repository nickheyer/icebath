//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package cliui

import "golang.org/x/sys/unix"

// Discards terminal input received but not yet read
func tcflushInput(fd int) { unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, 1) }
