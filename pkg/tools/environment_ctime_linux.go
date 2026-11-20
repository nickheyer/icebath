//go:build linux

package tools

import "syscall"

// Change time of a stat result in nanoseconds
func statCtimeNanos(sys *syscall.Stat_t) int64 {
	return sys.Ctim.Sec*1e9 + sys.Ctim.Nsec
}
