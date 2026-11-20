//go:build !windows

package gateway

import (
	"os"
	"os/exec"
	"syscall"
)

// Starts the detached diagnostic in its own session appending to logPath, false when it cannot start
func spawnAsyncDiagnostic(logPath, signalName string, timeoutSeconds float64) (int, bool) {
	script := asyncDiagnosticScript(signalName, os.Getpid())
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	_ = f.Chmod(0o600)
	argv := append(diagnosticTimeoutPrefix(timeoutSeconds), "bash", "-c", script)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, false
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, true
}
