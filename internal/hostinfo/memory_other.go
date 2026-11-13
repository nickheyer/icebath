//go:build !linux

package hostinfo

import "fmt"

// Sysinfo counters exist only on Linux
func ReadKernelMemory() (KernelMemory, error) {
	return KernelMemory{}, fmt.Errorf("kernel memory: %w", ErrAbsent)
}
