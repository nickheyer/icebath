package timeutil

import "golang.org/x/sys/windows"

// Time since boot from GetTickCount64, which counts sleep like the interrupt time the Go runtime reads here
func bootSeconds() (float64, bool) { return windows.DurationSinceBoot().Seconds(), true }
