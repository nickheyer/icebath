package pysubprocess

import "os/exec"

// Popen returncode, the unsigned DWORD exit code GetExitCodeProcess reports
func returncode(e *exec.ExitError) int { return e.ExitCode() }

// Windows signal.Signals members by number, the CTRL events included
var windowsSignals = map[int]string{1: "CTRL_BREAK_EVENT", 2: "SIGINT", 4: "SIGILL", 8: "SIGFPE", 11: "SIGSEGV", 15: "SIGTERM", 21: "SIGBREAK", 22: "SIGABRT"}

// signal.Signals name of a number on Windows, false when the enum has no such member
func signalName(n int) (string, bool) {
	name, ok := windowsSignals[n]
	return name, ok
}
