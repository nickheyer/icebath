//go:build freebsd || netbsd || openbsd || dragonfly

package terminal

import "golang.org/x/sys/unix"

// Request reading the termios of a terminal
const termiosRequest = unix.TIOCGETA
