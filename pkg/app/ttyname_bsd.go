//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package app

import "golang.org/x/sys/unix"

const ioctlReadTermios = unix.TIOCGETA
