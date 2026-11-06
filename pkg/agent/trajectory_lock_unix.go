//go:build !windows

package agent

import (
	"os"

	"golang.org/x/sys/unix"
)

// Exclusive whole file flock on an append handle, or its release
func lockAppendHandle(f *os.File, acquire bool) error {
	how := unix.LOCK_UN
	if acquire {
		how = unix.LOCK_EX
	}
	for {
		err := unix.Flock(int(f.Fd()), how)
		if err != unix.EINTR {
			return err
		}
	}
}
