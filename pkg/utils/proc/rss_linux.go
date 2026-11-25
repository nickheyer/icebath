package proc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Resident set size of pid in bytes, the statm resident pages times the page size
func RSS(pid int) (int64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNoSuchProcess
		}
		return 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed /proc/%d/statm", pid)
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * int64(os.Getpagesize()), nil
}
