package local

import (
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/hostinfo"
)

func ramBytes(*hostinfo.Host) (int64, int64) {
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		if total, available, ok := ramFromMeminfo(string(data)); ok {
			return total, available
		}
	}
	k, err := hostinfo.ReadKernelMemory()
	if err != nil {
		return 0, 0
	}
	total := int64(k.Total)
	available := int64(k.Free)
	if available == 0 {
		available = total / 2
	}
	return total, available
}

func ramFromMeminfo(text string) (int64, int64, bool) {
	fields := map[string]int64{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || (name != "MemTotal" && name != "MemAvailable" && name != "MemFree") {
			continue
		}
		parts := strings.Fields(value)
		if len(parts) != 2 || parts[1] != "kB" {
			continue
		}
		n, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		fields[name] = n * 1024
	}
	total, hasTotal := fields["MemTotal"]
	available, hasAvailable := fields["MemAvailable"]
	if !hasAvailable {
		available, hasAvailable = fields["MemFree"]
	}
	if !hasTotal || total <= 0 || !hasAvailable || available < 0 || available > total {
		return 0, 0, false
	}
	return total, available, true
}
