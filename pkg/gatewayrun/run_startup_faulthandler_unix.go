//go:build unix

package gatewayrun

import (
	"os"
	"syscall"
)

// Signal that dumps every goroutine stack to the faulthandler log
var stackDumpSignal os.Signal = syscall.SIGUSR2
