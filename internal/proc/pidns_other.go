//go:build !linux

package proc

import (
	"errors"
	"fmt"
	"runtime"
)

// Pid namespaces exist only on Linux, errors.ErrUnsupported here where a pid alone identifies a process
func PIDNamespace() (string, error) {
	return "", fmt.Errorf("pid namespace on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
