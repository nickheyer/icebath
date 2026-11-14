//go:build !windows

package selfupdate

import (
	"os"
	"syscall"
)

// Blocks on the exclusive flock of an open file, true since POSIX supports the blocking wait
func blockingFileLock(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return true, err
		}
	}
}
