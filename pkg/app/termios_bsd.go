//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package app

import "golang.org/x/sys/unix"

const (
	tcGets      = unix.TIOCGETA
	tcSetsFlush = unix.TIOCSETAF
)
