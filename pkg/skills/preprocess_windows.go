//go:build windows

package skills

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Hides the child console window the way windows_hide_flags does
func hideInlineShellWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

// Marker CreateProcess raises for a working directory that is missing or not a directory
func inlineShellCwdError(cwd string) string {
	if st, err := os.Stat(cwd); err == nil && st.IsDir() {
		return ""
	}
	return fmt.Sprintf("[inline-shell error: %s]", spawnErrorText(windows.ERROR_DIRECTORY, cwd))
}

// Popen returncode, the unsigned process exit status
func pyReturnCode(state *os.ProcessState) int { return state.ExitCode() }
