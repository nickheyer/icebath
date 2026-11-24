package timeutil

import "golang.org/x/sys/unix"

// CLOCK_MONOTONIC of NetBSD, a clock id golang.org/x/sys/unix leaves unnamed there
const netbsdClockMonotonic = 3

// CLOCK_MONOTONIC in seconds, the clock the Go runtime keeps its monotonic time with on NetBSD
func bootSeconds() (float64, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(netbsdClockMonotonic, &ts); err != nil {
		return 0, false
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9, true
}
