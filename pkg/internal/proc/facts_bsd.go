//go:build unix && !linux && !darwin

package proc

import (
	"strconv"
	"strings"
)

// Real uid of pid from the ps ruid column
func RealUID(pid int) (int, error) {
	row, err := psRow(pid, "ruid=")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(row))
}

// Kernel name of pid from the ps comm column
func comm(pid int) (string, error) {
	row, err := psRow(pid, "comm=")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(row), nil
}
