package hostinfo

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const meminfoPath = "/proc/meminfo"

// Physical memory and swap from meminfo, available memory estimated when the kernel states none or zero
func ReadMemory() (Memory, error) {
	data, err := os.ReadFile(meminfoPath)
	if err != nil {
		return Memory{}, fmt.Errorf("memory: %w", err)
	}
	m, ok := parseMeminfo(string(data))
	if !ok {
		return Memory{}, errors.New("memory: meminfo states no MemTotal or MemFree")
	}
	return m, nil
}

// Total of meminfo, the figure the machine summary rounds
func physicalTotal() (uint64, error) {
	data, err := os.ReadFile(meminfoPath)
	if err != nil {
		return 0, fmt.Errorf("memory: %w", err)
	}
	if total := meminfoFields(string(data))["MemTotal"]; total > 0 {
		return total, nil
	}
	return 0, errNoPhysicalMemory
}

// Total and free physical memory from sysinfo
func ReadKernelMemory() (KernelMemory, error) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return KernelMemory{}, fmt.Errorf("sysinfo: %w", err)
	}
	unit := max(uint64(info.Unit), 1)
	return KernelMemory{Total: uint64(info.Totalram) * unit, Free: uint64(info.Freeram) * unit}, nil
}

// Byte counts of meminfo text by field name
func meminfoFields(text string) map[string]uint64 {
	fields := map[string]uint64{}
	for _, line := range strings.Split(text, "\n") {
		name, rest, found := strings.Cut(line, ":")
		parts := strings.Fields(rest)
		if !found || len(parts) == 0 {
			continue
		}
		n, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			continue
		}
		if len(parts) > 1 && parts[1] == "kB" {
			n *= 1024
		}
		fields[name] = n
	}
	return fields
}

// Reading of meminfo text in bytes, false without MemTotal and MemFree
func parseMeminfo(text string) (Memory, bool) {
	fields := meminfoFields(text)
	total, hasTotal := fields["MemTotal"]
	free, hasFree := fields["MemFree"]
	if !hasTotal || !hasFree {
		return Memory{}, false
	}
	m := Memory{Total: total, Free: free}
	available, stated := fields["MemAvailable"]
	if stated {
		kernel := available
		m.KernelAvailable = &kernel
	}
	if !stated || available == 0 {
		available = estimateAvailable(fields)
	}
	if available > total {
		available = free
	}
	m.Available, m.Used = available, total-min(available, total)
	swapTotal, hasSwapTotal := fields["SwapTotal"]
	swapFree, hasSwapFree := fields["SwapFree"]
	if hasSwapTotal && hasSwapFree {
		m.Swap = &Swap{Total: swapTotal, Free: swapFree}
	}
	return m, true
}

// Kernel style estimate from free memory, page cache and reclaimable slab less the low watermarks
func estimateAvailable(fields map[string]uint64) uint64 {
	fallback := fields["MemFree"] + fields["Cached"]
	activeFile, hasActive := fields["Active(file)"]
	inactiveFile, hasInactive := fields["Inactive(file)"]
	slab, hasSlab := fields["SReclaimable"]
	if !hasActive || !hasInactive || !hasSlab {
		return fallback
	}
	data, err := os.ReadFile("/proc/zoneinfo")
	if err != nil {
		return fallback
	}
	var lowPages uint64
	for _, line := range strings.Split(string(data), "\n") {
		if parts := strings.Fields(line); len(parts) > 1 && parts[0] == "low" {
			n, _ := strconv.ParseUint(parts[1], 10, 64)
			lowPages += n
		}
	}
	low := float64(lowPages * uint64(os.Getpagesize()))
	pageCache := float64(activeFile + inactiveFile)
	available := float64(fields["MemFree"]) - low + pageCache - min(pageCache/2, low) + float64(slab) - min(float64(slab)/2, low)
	return uint64(max(available, 0))
}
