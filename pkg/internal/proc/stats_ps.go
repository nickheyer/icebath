//go:build unix && !linux && !darwin

package proc

import (
	"strconv"
	"strings"
)

// Virtual memory size of pid in bytes from the ps vsz column in kibibytes
func VMS(pid int) (int64, error) {
	row, err := psRow(pid, "vsz=")
	if err != nil {
		return 0, err
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(row), 10, 64)
	if err != nil {
		return 0, err
	}
	return kib * 1024, nil
}

// psutil status name of pid from the first letter of the ps stat column
func StatusName(pid int) (string, error) {
	letter, err := State(pid)
	if err != nil {
		return "", err
	}
	switch letter {
	case 'I':
		return "idle", nil
	case 'R', 'O':
		return "running", nil
	case 'T':
		return "stopped", nil
	case 'Z':
		return "zombie", nil
	case 'W':
		return "waiting", nil
	case 'L':
		return "locked", nil
	}
	return "sleeping", nil
}
