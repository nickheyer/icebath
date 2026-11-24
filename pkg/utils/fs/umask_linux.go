package fsutil

import (
	"os"
	"strconv"
	"strings"
)

// The process umask as /proc/self/status reports it without changing it, read by setting it on kernels that do not report it
func processUmask() uint32 {
	if data, err := os.ReadFile("/proc/self/status"); err == nil {
		for line := range strings.Lines(string(data)) {
			if rest, ok := strings.CutPrefix(line, "Umask:"); ok {
				if v, err := strconv.ParseUint(strings.TrimSpace(rest), 8, 32); err == nil {
					return uint32(v)
				}
				break
			}
		}
	}
	return swapUmask()
}
