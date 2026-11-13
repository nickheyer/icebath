package hostinfo

import (
	"fmt"
	"unsafe"
)

var procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")

// Counters GlobalMemoryStatusEx fills, the virtual address space ones left blank
type memoryStatusEx struct {
	length        uint32
	_             uint32
	totalPhys     uint64
	availPhys     uint64
	totalPageFile uint64
	availPageFile uint64
	_             [3]uint64
}

// Counters of one GlobalMemoryStatusEx call
func globalMemoryStatus() (memoryStatusEx, error) {
	var status memoryStatusEx
	if err := procGlobalMemoryStatusEx.Find(); err != nil {
		return status, fmt.Errorf("memory: %w", err)
	}
	status.length = uint32(unsafe.Sizeof(status))
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status))); r == 0 {
		return status, fmt.Errorf("memory: %w", err)
	}
	return status, nil
}

// Physical memory and commit from GlobalMemoryStatusEx
func ReadMemory() (Memory, error) {
	status, err := globalMemoryStatus()
	if err != nil {
		return Memory{}, err
	}
	return Memory{Total: status.totalPhys, Available: status.availPhys, Used: status.totalPhys - min(status.availPhys, status.totalPhys),
		Free: status.availPhys, Commit: &Commit{Limit: status.totalPageFile, Available: status.availPageFile}}, nil
}

// Physical memory size GlobalMemoryStatusEx reports
func physicalTotal() (uint64, error) {
	status, err := globalMemoryStatus()
	if err != nil {
		return 0, err
	}
	if status.totalPhys == 0 {
		return 0, errNoPhysicalMemory
	}
	return status.totalPhys, nil
}
