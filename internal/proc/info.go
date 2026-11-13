package proc

import (
	"strings"
	"time"
)

// Resource use of one process at the moment it was read
type Stats struct {
	// Resident set size in bytes, the working set on Windows
	RSS uint64
	// Virtual memory size in bytes, the committed page file usage on Windows
	VMS uint64
	// Threads the process runs
	Threads int
	// CPU time spent in user mode
	User time.Duration
	// CPU time spent in the kernel
	System time.Duration
	// Open file descriptors, open handles on Windows
	Descriptors int
}

// Splits a NUL separated environment block, stopping at an empty entry, with keys upper cased when fold is set
func parseEnviron(block string, fold bool) map[string]string {
	out := map[string]string{}
	pos := 0
	for {
		next := strings.IndexByte(block[pos:], 0)
		if next <= 0 {
			return out
		}
		next += pos
		if eq := strings.IndexByte(block[pos:next], '='); eq > 0 {
			key := block[pos : pos+eq]
			if fold {
				key = strings.ToUpper(key)
			}
			out[key] = block[pos+eq+1 : next]
		}
		pos = next + 1
	}
}
