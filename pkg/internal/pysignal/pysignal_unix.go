//go:build !windows

package pysignal

import (
	"os"
	"syscall"
	"time"
)

// Seconds the kernel default of a signal this process sent itself gets to end it
const defaultActionWait = 2 * time.Second

// Sends the signal to this process, the os.kill of its own pid
func KillSelf(sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return syscall.EINVAL
	}
	return syscall.Kill(os.Getpid(), s)
}

// Ends the process by SIGINT so its parent sees an interrupted child
func dieByInterrupt() {
	if KillSelf(os.Interrupt) == nil {
		time.Sleep(defaultActionWait)
	}
	os.Exit(130)
}
