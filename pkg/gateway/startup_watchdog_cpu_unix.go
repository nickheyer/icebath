//go:build !windows

package gateway

import "syscall"

// User plus system CPU seconds of this process across every thread, false when the probe fails
func processCPUSeconds() (float64, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	seconds := func(tv syscall.Timeval) float64 { return float64(tv.Sec) + float64(tv.Usec)/1e6 }
	return seconds(usage.Utime) + seconds(usage.Stime), true
}
