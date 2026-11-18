//go:build linux || solaris || illumos || aix

package app

import "golang.org/x/sys/unix"

const (
	tcGets      = unix.TCGETS
	tcSetsFlush = unix.TCSETSF
)
