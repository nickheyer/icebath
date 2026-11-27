package botdesktop

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	bdconfig "github.com/nickheyer/icebath/internal/botdesktop/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	cgroupV2 = "/sys/fs/cgroup"
	cgroupV1 = "/sys/fs/cgroup/memory"
	mib      = 1 << 20
)

// Memory free for a start and the limit it counts against, in MiB, -1 when unknown
type Memory struct {
	Available, Limit int
}

func trimmed(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(string(b), "\xef\xbb\xbf")), true
}

// Whole number a file holds, false for "max" or anything else
func fileInt(path string) (int64, bool) {
	text, ok := trimmed(path)
	if !ok || text == "" || strings.Trim(text, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(text, 10, 64)
	return n, err == nil
}

// Byte values of /proc/meminfo by field
func meminfo() map[string]int64 {
	text, ok := trimmed("/proc/meminfo")
	out := map[string]int64{}
	if !ok {
		return out
	}
	for _, line := range strings.Split(text, "\n") {
		key, rest, _ := strings.Cut(line, ":")
		if parts := strings.Fields(rest); len(parts) > 0 {
			if n, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
				out[key] = n * 1024
			}
		}
	}
	return out
}

// One value of a cgroup memory.stat, false when absent or unreadable
func statValue(path, key string) (int64, bool) {
	text, ok := trimmed(path)
	if !ok {
		return 0, false
	}
	for _, line := range strings.Split(text, "\n") {
		if name, rest, _ := strings.Cut(line, " "); name == key {
			n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// Limit of the cgroup this process runs in and its working set, the usage less the inactive page cache it could give back
func cgroup() (limit, usage int64, hasLimit, hasUsage bool) {
	limit, hasLimit = fileInt(filepath.Join(cgroupV2, "memory.max"))
	usage, hasUsage = fileInt(filepath.Join(cgroupV2, "memory.current"))
	cache, hasCache := statValue(filepath.Join(cgroupV2, "memory.stat"), "inactive_file")
	if !hasLimit && !hasUsage {
		limit, hasLimit = fileInt(filepath.Join(cgroupV1, "memory.limit_in_bytes"))
		usage, hasUsage = fileInt(filepath.Join(cgroupV1, "memory.usage_in_bytes"))
		cache, hasCache = statValue(filepath.Join(cgroupV1, "memory.stat"), "total_inactive_file")
		if hasLimit && limit >= 1<<60 {
			hasLimit = false
		}
	}
	if hasUsage && hasCache && cache != 0 {
		usage = max(usage-cache, 0)
	}
	return limit, usage, hasLimit, hasUsage
}

// The tighter of the machine's available memory and the cgroup's headroom, with the cgroup's limit or the machine's total
func memory() Memory {
	info := meminfo()
	limit, usage, hasLimit, hasUsage := cgroup()
	available, hasAvailable := info["MemAvailable"]
	if hasLimit && hasUsage {
		if headroom := limit - usage; !hasAvailable || headroom < available {
			available, hasAvailable = headroom, true
		}
	}
	if !hasLimit {
		limit, hasLimit = info["MemTotal"]
	}
	out := Memory{Available: -1, Limit: -1}
	if hasAvailable {
		out.Available = int(max(0, available) / mib)
	}
	if hasLimit {
		out.Limit = int(limit / mib)
	}
	return out
}

// Free memory a start needs, in MB, zero turning the check off
func minFree(cfg *config.Config) int {
	n := cfg.GetInt(bdconfig.MinFreeMemory)
	if _, err := strconv.Atoi(strings.TrimSpace(cfg.GetString(bdconfig.MinFreeMemory))); err != nil {
		return bdconfig.DefaultMinFreeMemory
	}
	return max(n, 0)
}

// Free memory below which a start goes ahead with a warning, a third above the floor
func tight(floor int) int { return floor + floor/3 }

// Why the screen must not start for lack of memory, empty when it may
func blocker(m Memory, need int) string {
	if need == 0 || m.Available < 0 || m.Available >= need {
		return ""
	}
	limit := ""
	if m.Limit > 0 {
		limit = " of " + strs.Counted(float64(m.Limit), "MB")
	}
	return fmt.Sprintf("There is not enough free memory for the agent desktop: %s is free%s, and it needs %s. Free some memory, or lower Memory "+
		"needed in /settings under Agent desktop.", strs.Counted(float64(m.Available), "MB"), limit, strs.Counted(float64(need), "MB"))
}
