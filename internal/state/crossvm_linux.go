package state

import (
	"os"
	"strconv"
	"strings"
)

// Filesystem type of the longest mount point containing dir per mountinfo, empty when unreadable or unmatched
func mountFSType(dir string) string {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	best, fstype := -1, ""
	for line := range strings.Lines(string(data)) {
		fields, tail, found := strings.Cut(strings.TrimRight(line, "\n"), " - ")
		parts := strings.Fields(fields)
		if !found || len(parts) < 5 || tail == "" {
			continue
		}
		mount := unescapeMount(parts[4])
		if dir == mount || strings.HasPrefix(dir, strings.TrimRight(mount, "/")+"/") {
			if len(mount) > best {
				best, fstype = len(mount), strings.Fields(tail)[0]
			}
		}
	}
	return strings.ToLower(fstype)
}

// Mount point with the octal escapes mountinfo writes for spaces, tabs, newlines and backslashes decoded
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
