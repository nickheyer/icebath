//go:build unix && !linux && !darwin && !freebsd && !openbsd && !netbsd

package proc

import (
	"fmt"
	"os"
)

// Descriptor counts are read from the procfs fd directory
const NumFDsAvailable = true

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
