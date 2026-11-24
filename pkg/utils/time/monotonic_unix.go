//go:build unix && !darwin && !netbsd && !aix && !hurd

package timeutil

import "golang.org/x/sys/unix"

// CLOCK_MONOTONIC in seconds, the clock the Go runtime keeps its monotonic time with on this platform
func bootSeconds() (float64, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, false
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9, true
}
