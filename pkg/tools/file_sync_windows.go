package tools

import (
	"os"
	"os/signal"

	"golang.org/x/sys/windows"
)

// Reports whether a process with the pid exists
func syncPidExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == 259
}

// Defers Ctrl+C until the returned restore runs, which re-delivers it to the console process group
func deferInterruptSignal(env *Env) func() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, os.Interrupt)
	return func() {
		signal.Stop(ch)
		select {
		case <-ch:
			toolsLogger(env).Debug("sync_back: SIGINT deferred until sync completes")
			windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0)
		default:
		}
	}
}
