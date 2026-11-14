package proc

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows processes hold handles, which psutil does not count as descriptors
const NumFDsAvailable = false

// Descriptor count Windows has no notion of
func NumFDs(pid int) (int, error) { return 0, errors.ErrUnsupported }

// Virtual memory size of pid in bytes, the pagefile usage the process status API reports
func VMS(pid int) (int64, error) {
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
	return int64(counters.pagefileUsage), nil
}

// Thread state and wait reason values of a suspended thread
const (
	threadStateWaiting  = 5
	waitReasonSuspended = 5
)

// psutil status name of pid, stopped when every thread waits suspended and running otherwise
func StatusName(pid int) (string, error) {
	threadSize, stateOff, reasonOff := 80, 68, 72
	if unsafe.Sizeof(uintptr(0)) == 4 {
		threadSize, stateOff, reasonOff = 64, 52, 56
	}
	size := uint32(512 << 10)
	for range 8 {
		buf := make([]byte, size)
		var ret uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), size, &ret)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			size = max(ret+64<<10, size*2)
			continue
		}
		if err != nil {
			return "", err
		}
		head := int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{}))
		for off := 0; off+head <= len(buf); {
			info := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
			if int(info.UniqueProcessID) == pid {
				threads := int(info.NumberOfThreads)
				for i := 0; i < threads; i++ {
					at := off + head + i*threadSize
					if at+threadSize > len(buf) {
						return "running", nil
					}
					state := *(*uint32)(unsafe.Pointer(&buf[at+stateOff]))
					reason := *(*uint32)(unsafe.Pointer(&buf[at+reasonOff]))
					if state != threadStateWaiting || reason != waitReasonSuspended {
						return "running", nil
					}
				}
				return "stopped", nil
			}
			if info.NextEntryOffset == 0 {
				break
			}
			off += int(info.NextEntryOffset)
		}
		return "", ErrNoSuchProcess
	}
	return "", fmt.Errorf("system process list kept outgrowing %d bytes", size)
}
