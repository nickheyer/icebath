//go:build !linux

package hostinfo

import "fmt"

// Cgroups exist only on Linux
func OwnCgroup() (string, error) { return "", fmt.Errorf("own cgroup: %w", ErrAbsent) }

// Cgroups exist only on Linux
func ReadCgroupMemory(string) (CgroupMemory, error) {
	return CgroupMemory{}, fmt.Errorf("cgroup memory: %w", ErrAbsent)
}

// Cgroups exist only on Linux
func ReadCgroupV1Memory() (CgroupMemory, error) {
	return CgroupMemory{}, fmt.Errorf("cgroup v1 memory: %w", ErrAbsent)
}
