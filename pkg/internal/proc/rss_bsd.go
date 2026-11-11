//go:build unix && !linux && !darwin

package proc

import (
	"strconv"
	"strings"
)

// Resident set size of pid in bytes from the ps rss column in kibibytes
func RSS(pid int) (int64, error) {
	row, err := psRow(pid, "rss=")
	if err != nil {
		return 0, err
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(row), 10, 64)
	if err != nil {
		return 0, err
	}
	return kib * 1024, nil
}
