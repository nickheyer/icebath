//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package tools

import "syscall"

// Change time of a stat result in nanoseconds
func statCtimeNanos(sys *syscall.Stat_t) int64 {
	return sys.Ctimespec.Sec*1e9 + sys.Ctimespec.Nsec
}
