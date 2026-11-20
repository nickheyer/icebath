//go:build !windows

package gateway

import (
	"errors"
	"os"
	"syscall"
)

var extraForwardedSignals = []os.Signal{syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2}

// Sends the signal to the child the way Popen.send_signal does, a child already gone ignored
func forwardSignal(p *os.Process, sig os.Signal) error {
	err := p.Signal(sig)
	if err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
