package proc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Descriptor counts are read from procfs
const NumFDsAvailable = true

// Virtual memory size of pid in bytes, the statm total pages times the page size
func VMS(pid int) (int64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNoSuchProcess
		}
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 1 {
		return 0, fmt.Errorf("malformed /proc/%d/statm", pid)
	}
	pages, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * int64(os.Getpagesize()), nil
}

// Count of descriptors pid holds open, the entries of its procfs fd directory
func NumFDs(pid int) (int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		switch {
		case os.IsNotExist(err):
			return 0, ErrNoSuchProcess
		case os.IsPermission(err):
			return 0, ErrAccessDenied
		}
		return 0, err
	}
	return len(entries), nil
}

// psutil status name of pid from its stat letter
func StatusName(pid int) (string, error) {
	letter, err := State(pid)
	if err != nil {
		return "", err
	}
	switch letter {
	case 'R':
		return "running", nil
	case 'S':
		return "sleeping", nil
	case 'D':
		return "disk-sleep", nil
	case 'T':
		return "stopped", nil
	case 't':
		return "tracing-stop", nil
	case 'Z':
		return "zombie", nil
	case 'X':
		return "dead", nil
	case 'K':
		return "wake-kill", nil
	case 'W':
		return "waking", nil
	case 'I':
		return "idle", nil
	case 'P':
		return "parked", nil
	}
	return "?", nil
}
