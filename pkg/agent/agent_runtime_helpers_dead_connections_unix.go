//go:build !windows

package agent

import (
	"errors"

	"golang.org/x/sys/unix"
)

// recv of one byte with MSG_PEEK and MSG_DONTWAIT, EOF or an OS error dead and EAGAIN healthy
func peekSocketDead(fd uintptr) (bool, error) {
	buf := make([]byte, 1)
	n, _, err := unix.Recvfrom(int(fd), buf, unix.MSG_PEEK|unix.MSG_DONTWAIT)
	if err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			return false, nil
		}
		return true, nil
	}
	return n == 0, nil
}
