//go:build windows

package gateway

import "golang.org/x/sys/windows"

// User plus kernel CPU seconds of this process across every thread, false when the probe fails
func processCPUSeconds() (float64, bool) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	ticks := func(ft windows.Filetime) float64 {
		return float64(uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime))
	}
	return (ticks(kernel) + ticks(user)) / 1e7, true
}
