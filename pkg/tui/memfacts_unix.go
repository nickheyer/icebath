//go:build linux || darwin

package tui

import "syscall"

// Peak resident size and CPU times the system accounts to the process, false when it does not answer
func resourceUsage() (processUsage, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return processUsage{}, false
	}
	micros := func(t syscall.Timeval) int64 { return int64(t.Sec)*1_000_000 + int64(t.Usec) }
	return processUsage{MaxRSS: uint64(ru.Maxrss) * maxRSSUnit, UserCPUTime: micros(ru.Utime), SystemCPUTime: micros(ru.Stime)}, true
}
