//go:build windows

package pysignal

import (
	"os"
	"syscall"
)

// Exit status of a console process ended by Ctrl C
var statusControlCExit uint32 = 0xC000013A

// Ends the process with the C runtime status of a raised signal that has no handler
func defaultAction(os.Signal) { os.Exit(3) }

// Ends this process with the signal number as its status, the os.kill of its own pid
func KillSelf(sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return syscall.EINVAL
	}
	os.Exit(int(s))
	return nil
}

// Ends the process with the status of an interrupted console process
func dieByInterrupt() { os.Exit(int(statusControlCExit)) }
