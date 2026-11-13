//go:build !windows

package hostinfo

import "fmt"

// Machine type attributes exist only on Windows
func RunnableArches() ([]Arch, error) {
	return nil, fmt.Errorf("runnable architectures: %w", ErrAbsent)
}
