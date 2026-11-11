//go:build !windows

package pysignal

import (
	"os"
	"syscall"
	"time"
)

// Seconds a raised default signal gets to end the process before the raise returns
const defaultActionWait = 2 * time.Second

// Delivers the signal to this process with no callable in its slot, the kernel default ending it
func defaultAction(sig os.Signal) {
	if KillSelf(sig) != nil {
		return
	}
	time.Sleep(defaultActionWait)
}

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
