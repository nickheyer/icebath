//go:build !linux && !windows

package updaterecord

import "runtime"

// Clock ticks per second sysconf SC_CLK_TCK reports, the libc CLK_TCK of the platform
func clockTicks() float64 {
	switch runtime.GOOS {
	case "freebsd", "dragonfly":
		return 128
	}
	return 100
}
