//go:build linux

package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Kibibytes the system counts the peak resident size of a process in
const maxRSSUnit = 1024

// Resident set size of the process in bytes from its statm row, zero when procfs does not answer
func residentBytes() uint64 {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}

// Resident bytes of the process that no file backs, zero when procfs does not answer
func anonymousBytes() uint64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, found := strings.CutPrefix(line, "RssAnon:"); found {
			kib, err := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
			if err != nil {
				return 0
			}
			return kib * 1024
		}
	}
	return 0
}

// Open file descriptors of the process, false when procfs does not answer
func openDescriptors() (int, bool) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	return len(entries), true
}

// Physical memory of the machine in bytes, zero when the system does not say
func physicalMemory() uint64 {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0
	}
	return uint64(info.Totalram) * uint64(info.Unit)
}

// Memory limit of the control group of the process in bytes, the lowest of the group and its parents, zero when none is set
func cgroupMemoryLimit() uint64 {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	var limit uint64
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		root, file := "/sys/fs/cgroup", "memory.max"
		switch {
		case parts[0] == "0" && parts[1] == "":
		case slices.Contains(strings.Split(parts[1], ","), "memory"):
			root, file = "/sys/fs/cgroup/memory", "memory.limit_in_bytes"
		default:
			continue
		}
		for dir := filepath.Join(root, parts[2]); strings.HasPrefix(dir, root); dir = filepath.Dir(dir) {
			text, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				continue
			}
			// An unlimited group reads "max" on the unified hierarchy and a number near the top of the range on the older one
			n, err := strconv.ParseUint(strings.TrimSpace(string(text)), 10, 64)
			if err == nil && n > 0 && n < 1<<62 && (limit == 0 || n < limit) {
				limit = n
			}
		}
	}
	return limit
}

// Memory map totals of the process as the kernel rolls them up, empty when procfs does not answer
func smapsRollup() string {
	raw, err := os.ReadFile("/proc/self/smaps_rollup")
	if err != nil {
		return ""
	}
	return string(raw)
}
