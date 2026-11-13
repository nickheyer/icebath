package proc

import (
	"os"
	"syscall"
)

// Signal numbers the Windows C runtime defines, the two console control events included
var windowsSignals = map[syscall.Signal]string{
	0: "CTRL_C_EVENT", 1: "CTRL_BREAK_EVENT", 2: "SIGINT", 4: "SIGILL", 8: "SIGFPE",
	11: "SIGSEGV", 15: "SIGTERM", 21: "SIGBREAK", 22: "SIGABRT",
}

// Symbolic name of sig such as SIGTERM, false for a number the Windows C runtime does not define
func SignalName(sig syscall.Signal) (string, bool) {
	name, ok := windowsSignals[sig]
	return name, ok
}

// Outcome of a Windows process, its exit code being the whole unsigned status GetExitCodeProcess reports
func exitOf(state *os.ProcessState) Exit { return Exit{Code: state.ExitCode()} }
