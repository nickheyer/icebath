//go:build !windows

package tuigateway

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// fcntl exists on this platform
func stdinRecoverable() bool { return true }

// Whether the shared open file description of fd 0 carries O_NONBLOCK
func stdinNonblock() bool {
	flags, err := unix.FcntlInt(0, unix.F_GETFL, 0)
	return err == nil && flags&unix.O_NONBLOCK != 0
}

// O_NONBLOCK flag of fd 0, or the F_GETFL failure
func stdinFlagParts() []string {
	flags, err := unix.FcntlInt(0, unix.F_GETFL, 0)
	if err != nil {
		return []string{fmt.Sprintf("F_GETFL error: %s", err)}
	}
	if flags&unix.O_NONBLOCK != 0 {
		return []string{"O_NONBLOCK=1"}
	}
	return []string{"O_NONBLOCK=0"}
}

// Integer read of SO_RCVTIMEO on a dup of fd 0, false when fd 0 is no socket
func stdinRcvTimeo() (int, bool) {
	fd, err := unix.Dup(0)
	if err != nil {
		return 0, false
	}
	defer unix.Close(fd)
	v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Puts fd 0 back in blocking mode and clears its receive timeout
func restoreStdinBlocking() {
	_ = unix.SetNonblock(0, false)
	fd, err := unix.Dup(0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{})
}
