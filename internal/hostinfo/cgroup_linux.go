package hostinfo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	cgroupRoot        = "/sys/fs/cgroup"
	cgroupV1Memory    = "/sys/fs/cgroup/memory"
	cgroupUnlimitedAt = 1 << 60
)

// Cgroup v2 path of this process relative to the unified hierarchy, ErrAbsent without one
func OwnCgroup() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("own cgroup: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok && strings.TrimSpace(rest) != "" {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("own cgroup: %w", ErrAbsent)
}

// Memory controller files of a cgroup v2 path such as the one OwnCgroup returns, slash for the root
func ReadCgroupMemory(cgroup string) (CgroupMemory, error) {
	dir := filepath.Join(cgroupRoot, cgroup)
	stat := cgroupStat(filepath.Join(dir, "memory.stat"))
	return CgroupMemory{
		High:         cgroupValue(filepath.Join(dir, "memory.high")),
		Max:          cgroupValue(filepath.Join(dir, "memory.max")),
		Current:      cgroupValue(filepath.Join(dir, "memory.current")),
		Anon:         stat["anon"],
		InactiveFile: stat["inactive_file"],
	}, nil
}

// Memory controller files of the cgroup v1 hierarchy root
func ReadCgroupV1Memory() (CgroupMemory, error) {
	stat := cgroupStat(filepath.Join(cgroupV1Memory, "memory.stat"))
	return CgroupMemory{
		Max:          cgroupValue(filepath.Join(cgroupV1Memory, "memory.limit_in_bytes")),
		Current:      cgroupValue(filepath.Join(cgroupV1Memory, "memory.usage_in_bytes")),
		InactiveFile: stat["total_inactive_file"],
	}, nil
}

// Byte count a cgroup file holds, nil when unreadable, max or the v1 unlimited sentinel
func cgroupValue(path string) *uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	n, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(string(data), "\xEF\xBB\xBF")), 10, 64)
	if err != nil || n >= cgroupUnlimitedAt {
		return nil
	}
	return &n
}

// Key and byte count pairs of a memory.stat file, empty when unreadable
func cgroupStat(path string) map[string]*uint64 {
	values := map[string]*uint64{}
	data, err := os.ReadFile(path)
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64); err == nil {
			values[key] = &n
		}
	}
	return values
}
