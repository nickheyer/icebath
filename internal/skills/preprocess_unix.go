//go:build !windows

package skills

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Marker for a working folder a snippet can't start in, none when it can: a missing one reads as bash not found, as starting there does
func badFolder(dir string) string {
	info, err := os.Stat(dir)
	if err == nil && !info.IsDir() {
		err = syscall.ENOTDIR
	}
	if err == nil {
		err = syscall.Access(dir, 1)
	}
	if err == nil {
		return ""
	}
	if errors.Is(err, os.ErrNotExist) {
		return "[inline-shell error: bash not found]"
	}
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return fmt.Sprintf("[inline-shell error: %s: %s]", errno.Error(), dir)
	}
	return fmt.Sprintf("[inline-shell error: %v]", err)
}
