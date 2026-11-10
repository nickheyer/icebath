//go:build unix

package app

import (
	"os"

	"golang.org/x/sys/unix"
)

// Reports whether the file has bytes ready to read right now
func fileReadable(f *os.File) bool {
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	return err == nil && n > 0 && fds[0].Revents&unix.POLLIN != 0
}
