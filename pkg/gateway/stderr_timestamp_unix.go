//go:build !windows

package gateway

import (
	"os"
	"syscall"
)

var extraForwardedSignals = []os.Signal{syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2}
