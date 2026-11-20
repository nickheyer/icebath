//go:build unix

package gateway

import (
	"errors"
	"syscall"
)

const hasSIGUSR1 = true

var errProcessGone = errors.New("process lookup failed")

// Sends SIGUSR1 to pid, errProcessGone when it no longer exists
func sendSIGUSR1(pid int) error {
	err := syscall.Kill(pid, syscall.SIGUSR1)
	if errors.Is(err, syscall.ESRCH) {
		return errProcessGone
	}
	return err
}
