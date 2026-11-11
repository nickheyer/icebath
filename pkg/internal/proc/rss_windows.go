package proc

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

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

// Resident set size of pid in bytes, the working set size the process status API reports
func RSS(pid int) (int64, error) {
	h, err := OpenProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_VM_READ)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var counters processMemoryCounters
	counters.cb = uint32(unsafe.Sizeof(counters))
	r, _, callErr := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&counters)), uintptr(counters.cb))
	if r == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
			return 0, errno
		}
		return 0, syscall.EINVAL
	}
	return int64(counters.workingSetSize), nil
}
