package proc

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	procIsProcessInJob        = kernel32.NewProc("IsProcessInJob")
)

// Whether pid names a running process, a refused handle meaning another user owns a live one, without signalling it
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	return !handleExited(h)
}

// Whether pid is listed as a process at all, the idle process zero included
func Exists(pid int) bool {
	switch {
	case pid < 0:
		return false
	case pid == 0:
		return true
	}
	return Alive(pid)
}

// Ends this process with the signal number as its exit status, the code a terminated Windows process reports
func ExitBySignal(sig syscall.Signal) { os.Exit(int(sig)) }

// Console control handler absorbing Ctrl+C and Ctrl+Break, created once since callbacks are never freed
var absorbControls = sync.OnceValue(func() uintptr {
	return windows.NewCallback(func(event uint32) uintptr {
		if event == windows.CTRL_C_EVENT || event == windows.CTRL_BREAK_EVENT {
			return 1
		}
		return 0
	})
})

// Makes this process ignore Ctrl+C and Ctrl+Break from any console it shares, as detached services must
func IgnoreConsoleControls() error {
	if r, _, err := procSetConsoleCtrlHandler.Call(0, 1); r == 0 {
		return fmt.Errorf("ignore Ctrl+C: %w", err)
	}
	if r, _, err := procSetConsoleCtrlHandler.Call(absorbControls(), 1); r == 0 {
		return fmt.Errorf("absorb Ctrl+Break: %w", err)
	}
	return nil
}
