//go:build windows

package tui

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetProcessMemoryInfo  = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")
	procGetProcessHandleCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")
	procGlobalMemoryStatusEx  = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
)

// PROCESS_MEMORY_COUNTERS of the process status API
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// MEMORYSTATUSEX of the system information API
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// Memory counters of the process, false when the process status API does not answer
func processCounters() (processMemoryCounters, bool) {
	var counters processMemoryCounters
	counters.cb = uint32(unsafe.Sizeof(counters))
	r, _, _ := procGetProcessMemoryInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&counters)), uintptr(counters.cb))
	return counters, r != 0
}

// Working set of the process in bytes, zero when the process status API does not answer
func residentBytes() uint64 {
	counters, ok := processCounters()
	if !ok {
		return 0
	}
	return uint64(counters.workingSetSize)
}

// Resident bytes of the process that no file backs, unknown on a system whose working set counters do not split them out
func anonymousBytes() uint64 { return 0 }

// Open handles of the process, the nearest thing Windows has to file descriptors, false when the system does not answer
func openDescriptors() (int, bool) {
	var count uint32
	r, _, _ := procGetProcessHandleCount.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&count)))
	return int(count), r != 0
}

// Peak working set and CPU times the system accounts to the process, false when it does not answer
func resourceUsage() (processUsage, bool) {
	counters, ok := processCounters()
	if !ok {
		return processUsage{}, false
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return processUsage{}, false
	}
	// A file time counts intervals of 100 nanoseconds
	micros := func(t windows.Filetime) int64 { return (int64(t.HighDateTime)<<32 | int64(t.LowDateTime)) / 10 }
	return processUsage{MaxRSS: uint64(counters.peakWorkingSetSize), UserCPUTime: micros(user), SystemCPUTime: micros(kernel)}, true
}

// Physical memory of the machine in bytes, zero when the system does not say
func physicalMemory() uint64 {
	var status memoryStatusEx
	status.length = uint32(unsafe.Sizeof(status))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status))); r == 0 {
		return 0
	}
	return status.totalPhys
}

// Memory limit of a control group, none on a system without control groups
func cgroupMemoryLimit() uint64 { return 0 }

// Memory map totals of the process, empty on a system without procfs
func smapsRollup() string { return "" }
