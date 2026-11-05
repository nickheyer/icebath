//go:build !windows

package pm

import (
	"os"

	"golang.org/x/sys/unix"
)

// Takes the protocol pipes off the standard streams so every child sees stderr for stdout and EOF for stdin
func workerStdio() (controls, wire *os.File, err error) {
	wireFD, err := unix.FcntlInt(1, unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.Dup2(2, 1); err != nil {
		return nil, nil, err
	}
	controlsFD, err := unix.FcntlInt(0, unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, err
	}
	defer null.Close()
	if err := unix.Dup2(int(null.Fd()), 0); err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(controlsFD), "pm-controls"), os.NewFile(uintptr(wireFD), "pm-wire"), nil
}

// Asks a worker still running after the exchange to stop with SIGTERM
func terminateWorker(p *os.Process) {
	_ = p.Signal(unix.SIGTERM)
}
