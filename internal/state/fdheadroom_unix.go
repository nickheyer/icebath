//go:build unix

package state

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Soft descriptor limit of the process, false when unlimited or unreadable
func fdSoftLimit() (int64, bool) {
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rl); err != nil || rl.Cur == unix.RLIM_INFINITY {
		return 0, false
	}
	return int64(rl.Cur), true
}

// Open descriptors of this process, -1 when the count itself hit the limit, false when no descriptor table lists
func openFDCount() (int, bool) {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(dir)
		if err == nil {
			return len(entries), true
		}
		if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
			return -1, true
		}
	}
	return 0, false
}
