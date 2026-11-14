//go:build !linux && !windows

package process

import "golang.org/x/sys/unix"

const ioctlGetTermios = unix.TIOCGETA
