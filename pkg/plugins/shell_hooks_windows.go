//go:build windows

package plugins

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Hook child starts without a console window
func shellHookProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// Resolves a bare command the way CreateProcess searches PATH
func shellHookExecutable(name string, env map[string]string) (string, error) {
	return exec.LookPath(name)
}

// Popen.returncode of a finished child
func shellHookReturnCode(ps *os.ProcessState) int { return ps.ExitCode() }

// Whether a start error is WinError 193, a file CreateProcess cannot run
func isBadExeFormat(err error) bool { return errors.Is(err, windows.ERROR_BAD_EXE_FORMAT) }

// os.access on Windows answers existence for R_OK and X_OK alike
func shellHookAccess(path string, exec bool) bool {
	_, err := os.Stat(path)
	return err == nil
}
