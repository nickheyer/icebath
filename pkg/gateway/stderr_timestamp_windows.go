//go:build windows

package gateway

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"golang.org/x/sys/windows"
)

var extraForwardedSignals []os.Signal

// Terminates the child for SIGTERM the way Popen.send_signal does, any other signal an unsupported ValueError
func forwardSignal(p *os.Process, sig os.Signal) error {
	if errors.Is(p.Signal(sig), os.ErrProcessDone) {
		return nil
	}
	if sig != syscall.SIGTERM {
		return &pyrepr.FormatError{Class: "ValueError", Msg: fmt.Sprintf("Unsupported signal: %d", sig)}
	}
	err := p.Kill()
	if err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) && !childStillActive(p.Pid) {
		return nil
	}
	return err
}

// Reports whether the exit code of the process still reads STILL_ACTIVE
func childStillActive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return true
	}
	return code == stillActive
}

// Exit code GetExitCodeProcess reports for a process that has not exited
const stillActive = 259
