//go:build unix

package gateway

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// RSS high water mark from getrusage, KB on Linux and bytes on macOS, else the current resident set
func readProcessRSSMB() (int, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		div := int64(1024)
		if runtime.GOOS == "darwin" {
			div = memoryBytesToMB
		}
		return int(int64(ru.Maxrss) / div), true
	}
	return currentRSSMB()
}

// Current resident set in MB from the resident page count of the process
func currentRSSMB() (int, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return int(pages * int64(os.Getpagesize()) / memoryBytesToMB), true
}
