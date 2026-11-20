package botdesktop

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	cgroupV2 = "/sys/fs/cgroup"
	cgroupV1 = "/sys/fs/cgroup/memory"
	meminfoP = "/proc/meminfo"
)

const mib = 1024 * 1024

// DefaultMinFreeMB is the memory floor a screen start needs when config sets none
const DefaultMinFreeMB = 1536

// Available memory and the limit in MiB, nil when unreadable
type MemoryInfo struct {
	AvailableMB *int
	LimitMB     *int
}

// File text with a BOM dropped and whitespace trimmed
func readTrimmed(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff")), true
}

// Non negative integer content of a file, nil for max or anything else
func readInt(path string) *int64 {
	text, ok := readTrimmed(path)
	if !ok || text == "" || strings.Trim(text, "0123456789") != "" {
		return nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// Byte values of /proc/meminfo keyed by field name
func meminfo() map[string]int64 {
	text, ok := readTrimmed(meminfoP)
	if !ok {
		return map[string]int64{}
	}
	out := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		key, rest, _ := strings.Cut(line, ":")
		parts := strings.Fields(rest)
		if len(parts) > 0 && strings.Trim(parts[0], "0123456789") == "" {
			if n, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
				out[key] = n * 1024
			}
		}
	}
	return out
}

// One key value line of a cgroup memory.stat, nil when absent or unreadable
func statValue(path, key string) *int64 {
	text, ok := readTrimmed(path)
	if !ok {
		return nil
	}
	for _, line := range strings.Split(text, "\n") {
		name, rest, _ := strings.Cut(line, " ")
		if name == key {
			n, ok := pyint.Parse(strings.TrimSpace(rest))
			if !ok || !n.IsInt64() {
				return nil
			}
			v := n.Int64()
			return &v
		}
	}
	return nil
}

// Cgroup limit and working set, usage minus reclaimable inactive page cache
func cgroupLimitAndUsage() (*int64, *int64) {
	limit := readInt(filepath.Join(cgroupV2, "memory.max"))
	usage := readInt(filepath.Join(cgroupV2, "memory.current"))
	cache := statValue(filepath.Join(cgroupV2, "memory.stat"), "inactive_file")
	if limit == nil && usage == nil {
		limit = readInt(filepath.Join(cgroupV1, "memory.limit_in_bytes"))
		usage = readInt(filepath.Join(cgroupV1, "memory.usage_in_bytes"))
		cache = statValue(filepath.Join(cgroupV1, "memory.stat"), "total_inactive_file")
		if limit != nil && *limit >= 1<<60 {
			limit = nil
		}
	}
	if usage != nil && cache != nil && *cache != 0 {
		u := max(*usage-*cache, 0)
		usage = &u
	}
	return limit, usage
}

// Tighter of host MemAvailable and the cgroup headroom, with the cgroup limit or MemTotal
func GetMemoryInfo() MemoryInfo {
	info := meminfo()
	limit, usage := cgroupLimitAndUsage()
	var available *int64
	if v, ok := info["MemAvailable"]; ok {
		available = &v
	}
	if limit != nil && usage != nil {
		headroom := *limit - *usage
		if available == nil || headroom < *available {
			available = &headroom
		}
	}
	limitBytes := limit
	if limitBytes == nil {
		if v, ok := info["MemTotal"]; ok {
			limitBytes = &v
		}
	}
	var out MemoryInfo
	if available != nil {
		mb := int(max(0, floorDiv(*available, mib)))
		out.AvailableMB = &mb
	}
	if limitBytes != nil {
		mb := int(floorDiv(*limitBytes, mib))
		out.LimitMB = &mb
	}
	return out
}

// Python floor division of int64 values
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// Configured bot_desktop.min_free_memory_mb, zero disabling the gate
func MinFreeMB(env *tools.Env) (int, error) {
	cfg, err := botDesktopSection(env)
	if err != nil {
		return 0, err
	}
	var raw any = DefaultMinFreeMB
	if v, ok := cfg["min_free_memory_mb"]; ok {
		raw = v
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return DefaultMinFreeMB, nil
	}
	if n.Sign() < 0 {
		return 0, nil
	}
	if !n.IsInt64() || n.Cmp(big.NewInt(int64(^uint(0)>>1))) > 0 {
		return int(^uint(0) >> 1), nil
	}
	return int(n.Int64()), nil
}

// Free memory below which a start is allowed and logged, a third above the floor
func TightHeadroomMB(floor int) int {
	return floor + floor/3
}

// Why the screen must not start now for lack of memory, empty when it may
func MemoryBlocker(info MemoryInfo, need int) string {
	if need == 0 {
		return ""
	}
	if info.AvailableMB == nil || *info.AvailableMB >= need {
		return ""
	}
	limit := ""
	if info.LimitMB != nil && *info.LimitMB != 0 {
		limit = fmt.Sprintf(" of %d MB", *info.LimitMB)
	}
	return fmt.Sprintf("Not enough free memory for a desktop: %d MB available%s, %d MB needed (bot_desktop.min_free_memory_mb)",
		*info.AvailableMB, limit, need)
}
