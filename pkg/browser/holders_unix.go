//go:build !windows

package browser

import (
	"errors"
	"os"
	"syscall"
)

const oNoFollow = syscall.O_NOFOLLOW

func pidNamespaceInode() (uint64, bool) {
	info, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Ino), true
}

// Takes an exclusive flock, false when non blocking and another process holds it
func lockFile(f *os.File, records string, blocking bool) (bool, error) {
	how := syscall.LOCK_EX
	if !blocking {
		how |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
}

func unlockFile(f *os.File) {}
