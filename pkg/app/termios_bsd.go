//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package app

import "golang.org/x/sys/unix"

const (
	tcGets      = unix.TIOCGETA
	tcSetsNow   = unix.TIOCSETA
	tcSetsFlush = unix.TIOCSETAF
)
