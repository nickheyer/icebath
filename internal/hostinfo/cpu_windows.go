package hostinfo

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	cpuRegistryKey                 = `HARDWARE\DESCRIPTION\System\CentralProcessor\0`
	allProcessorGroups             = 0xFFFF
	systemProcessorPerformanceInfo = 8
	hundredNanoseconds             = 1e-7
	cpuWindowsIdle                 = 2
)

var procGetSystemTimes = kernel32.NewProc("GetSystemTimes")

// Per processor times NtQuerySystemInformation reports in hundred nanosecond units
type processorPerformance struct {
	_              [3]int64
	dpc, interrupt int64
	_              uint32
}

// String value under the local machine hive trimmed, empty when missing or not a string
func registryString(path, name string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	value, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// Processor name string of the first processor in the registry
func cpuModel() string { return registryString(cpuRegistryKey, "ProcessorNameString") }

// Vendor identifier of the first processor in the registry, the SoC vendor on arm64
func cpuVendor(*Host) string { return registryString(cpuRegistryKey, "VendorIdentifier") }

// Logical processors active across every processor group
func CPUCount() (int, error) {
	count := windows.GetActiveProcessorCount(allProcessorGroups)
	if count == 0 {
		return 0, errors.New("active processors: none reported")
	}
	return int(count), nil
}

// Active processors, since this process keeps the default affinity of every group, at least one
func UsableCPUs() int {
	count, err := CPUCount()
	if err != nil {
		return 1
	}
	return max(1, count)
}

// Seconds of a FILETIME counter
func filetimeSeconds(ft windows.Filetime) float64 {
	return float64(uint64(ft.HighDateTime)<<32|uint64(ft.LowDateTime)) * hundredNanoseconds
}

// Seconds of the host CPU in the order user system idle interrupt dpc, system excluding idle
func cpuTimes() ([]float64, error) {
	var idle, kernel, user windows.Filetime
	if r, _, err := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return nil, fmt.Errorf("cpu times: %w", err)
	}
	count := windows.GetActiveProcessorCount(allProcessorGroups)
	if count == 0 {
		return nil, errors.New("cpu times: no active processor")
	}
	infos := make([]processorPerformance, count)
	entry := uint32(unsafe.Sizeof(infos[0]))
	var returned uint32
	if err := windows.NtQuerySystemInformation(systemProcessorPerformanceInfo, unsafe.Pointer(&infos[0]), entry*count, &returned); err != nil {
		return nil, fmt.Errorf("cpu times: %w", err)
	}
	var interrupt, dpc float64
	for _, info := range infos[:min(count, returned/entry)] {
		interrupt += float64(info.interrupt) * hundredNanoseconds
		dpc += float64(info.dpc) * hundredNanoseconds
	}
	idleSeconds := filetimeSeconds(idle)
	return []float64{filetimeSeconds(user), filetimeSeconds(kernel) - idleSeconds, idleSeconds, interrupt, dpc}, nil
}

// Every field counted in the total, busy time less idle
func cpuTotalBusy(d []float64) (total, busy float64) {
	for _, v := range d {
		total += v
	}
	return total, total - d[cpuWindowsIdle]
}
