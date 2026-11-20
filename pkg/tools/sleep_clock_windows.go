//go:build windows

package tools

import (
	"syscall"
	"unsafe"
)

// Unbiased interrupt time in seconds, nil where kernel32 lacks QueryUnbiasedInterruptTime
func windowsUnbiasedClock() func() float64 {
	query := syscall.NewLazyDLL("kernel32.dll").NewProc("QueryUnbiasedInterruptTime")
	if query.Find() != nil {
		return nil
	}
	return func() float64 {
		var value uint64
		query.Call(uintptr(unsafe.Pointer(&value)))
		return float64(value) / 10_000_000
	}
}
