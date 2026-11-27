//go:build linux

package terminal

import "golang.org/x/sys/unix"

// Request reading the termios of a terminal
const termiosRequest = unix.TCGETS
