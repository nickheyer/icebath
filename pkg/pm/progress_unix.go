//go:build !windows

package pm

import (
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Name Python's signal module gives a signal number on this host, empty when the number has none
func signalName(n int) string {
	if name := unix.SignalName(syscall.Signal(n)); name != "" {
		return name
	}
	if runtime.GOOS != "linux" {
		return ""
	}
	if n == 64 {
		return "SIGRTMAX"
	}
	target, err := CurrentTarget()
	musl := err == nil && MuslTargets[target]
	bionic := err == nil && strings.HasSuffix(target, "-bionic")
	if (n == 34 && !musl && !bionic) || (n == 35 && musl) {
		return "SIGRTMIN"
	}
	return ""
}
