//go:build solaris || illumos

package app

import "golang.org/x/sys/unix"

const ioctlReadTermios = unix.TCGETS
