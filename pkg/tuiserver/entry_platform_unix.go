//go:build !windows

package tuiserver

import (
	"os"
	"syscall"
)

// Names the termination signals of the entry are logged under
var entrySignalNames = map[os.Signal]string{syscall.SIGPIPE: "SIGPIPE", syscall.SIGTERM: "SIGTERM",
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT"}
