//go:build !windows

package tuigateway

import (
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
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
		return []string{"F_GETFL error: " + pypath.OSErrorText(err)}
	}
	if flags&unix.O_NONBLOCK != 0 {
		return []string{"O_NONBLOCK=1"}
	}
	return []string{"O_NONBLOCK=0"}
}

// Result of getter run on a dup of fd 0, false when the dup or the getter fails
func stdinSockopt(getter func(fd int) (int, error)) (int, bool) {
	fd, err := unix.Dup(0)
	if err != nil {
		return 0, false
	}
	defer unix.Close(fd)
	v, err := getter(fd)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Integer read of SO_RCVTIMEO on a dup of fd 0, false when fd 0 is no socket
func stdinRcvTimeo() (int, bool) {
	return stdinSockopt(func(fd int) (int, error) { return unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO) })
}

// Puts fd 0 back in blocking mode, raising when that fails, and clears its receive timeout
func restoreStdinBlocking() {
	if err := unix.SetNonblock(0, false); err != nil {
		panic(&os.SyscallError{Syscall: "fcntl", Err: err})
	}
	stdinSockopt(func(fd int) (int, error) {
		return 0, unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{})
	})
}
