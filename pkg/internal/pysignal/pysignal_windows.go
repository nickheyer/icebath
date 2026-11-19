//go:build windows

package pysignal

import "os"

// Exit status of a console process ended by Ctrl C
var statusControlCExit uint32 = 0xC000013A

// Ends the process with the status of an interrupted console process
func dieByInterrupt() { os.Exit(int(statusControlCExit)) }
