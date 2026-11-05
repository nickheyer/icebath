//go:build windows

package sleepclock

import (
	"syscall"
	"unsafe"
)

// Unbiased interrupt time in seconds, the monotonic clock when kernel32 lacks it
func awakeClock() func() float64 {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("QueryUnbiasedInterruptTime")
	if proc.Find() != nil {
		return monotonicSeconds
	}
	return func() float64 {
		var value uint64
		proc.Call(uintptr(unsafe.Pointer(&value)))
		return float64(value) / 10_000_000
	}
}
