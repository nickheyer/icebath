//go:build unix

package pythread

import "golang.org/x/sys/unix"

// CLOCK_MONOTONIC reading in seconds at process start, zero when the clock cannot be read
func systemMonotonic() float64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9
}
