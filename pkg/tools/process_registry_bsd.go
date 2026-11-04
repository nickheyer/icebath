//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package tools

import "golang.org/x/sys/unix"

// Physical memory of the host in bytes
func physicalMemoryBytes() (int64, bool) {
	for _, name := range []string{"hw.memsize", "hw.physmem"} {
		if v, err := unix.SysctlUint64(name); err == nil && v > 0 {
			return int64(v), true
		}
	}
	return 0, false
}
