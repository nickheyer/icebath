//go:build windows

package pythread

import (
	"syscall"
	"unsafe"
)

// QueryPerformanceCounter reading in seconds at process start, zero when the counter cannot be read
func systemMonotonic() float64 {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	counter, frequency := kernel32.NewProc("QueryPerformanceCounter"), kernel32.NewProc("QueryPerformanceFrequency")
	if counter.Find() != nil || frequency.Find() != nil {
		return 0
	}
	var ticks, perSecond int64
	frequency.Call(uintptr(unsafe.Pointer(&perSecond)))
	counter.Call(uintptr(unsafe.Pointer(&ticks)))
	if perSecond <= 0 {
		return 0
	}
	return float64(ticks) / float64(perSecond)
}
