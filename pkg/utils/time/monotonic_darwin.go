package timeutil

import "golang.org/x/sys/unix"

// CLOCK_UPTIME_RAW in seconds, the mach_absolute_time count the Go runtime keeps its monotonic time with here
func bootSeconds() (float64, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_UPTIME_RAW, &ts); err != nil {
		return 0, false
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9, true
}
