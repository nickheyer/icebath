//go:build !windows

package gateway

import (
	"os/signal"
	"syscall"
)

// Ignores SIGHUP for this process and the children that inherit its dispositions
func ignoreHangupSignal() { signal.Ignore(syscall.SIGHUP) }
