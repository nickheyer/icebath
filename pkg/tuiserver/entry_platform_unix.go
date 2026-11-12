//go:build !windows

package tuiserver

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/internal/pysignal"
)

// Names the termination signals of the entry are logged under
var entrySignalNames = map[os.Signal]string{syscall.SIGPIPE: "SIGPIPE", syscall.SIGTERM: "SIGTERM",
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT"}

// Drops SIGPIPE so a dead pipe fails the write, ignores SIGINT and routes SIGTERM and SIGHUP to the logged exit
func installEntrySignals(e *stdioEntry) {
	pysignal.Signal(syscall.SIGPIPE, pysignal.Func(func(os.Signal) error { return nil }))
	pysignal.Signal(syscall.SIGTERM, pysignal.Func(e.logSignal))
	pysignal.Signal(syscall.SIGHUP, pysignal.Func(e.logSignal))
	pysignal.Signal(os.Interrupt, pysignal.Ignore)
}

// Marks the RPC stdin close on exec so a child started without its own stdin cannot read requests
func closeRPCStdinOnExec() {
	file, hasFD := entryStdin.(interface{ Fd() uintptr })
	if !hasFD {
		entryLogger().Debug("could not mark TUI RPC stdin close-on-exec", "exc_info", "stdin has no file descriptor")
		return
	}
	fd := int(file.Fd())
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err == nil {
		_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC)
	}
	if err != nil {
		entryLogger().Debug("could not mark TUI RPC stdin close-on-exec", "exc_info", err.Error())
	}
}
