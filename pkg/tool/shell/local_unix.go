//go:build !windows

package shell

import (
	"os/exec"
	"path/filepath"
	"syscall"
)

// Script run by the shell so the final directory reaches the directory file however the command ends
const wrapper = `trap 'pwd -P > "$2"' EXIT
eval "$1"`

// Bash script that also writes the exit code of every pipeline stage on a second line
const bashWrapper = `trap 'pwd -P > "$2"; printf "%s\n" "${__icebath_ps[*]}" >> "$2"' EXIT
eval "$1"$'\n''__icebath_ps=("${PIPESTATUS[@]}")'
exit "${__icebath_ps[${#__icebath_ps[@]}-1]}"`

func (l Local) shell() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "sh"
}

// Arguments that run line inside the wrapper and report the final directory into dirFile
func (l Local) args(shell, line, dirFile string) []string {
	script := wrapper
	if filepath.Base(shell) == "bash" {
		script = bashWrapper
	}
	return []string{"-c", script, shell, line, dirFile}
}

// Shell style status, 128 plus the signal number when the process was killed
func exitCode(c *exec.Cmd, err error, killed bool) int {
	if err == nil {
		return 0
	}
	if ws, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if killed {
		return 137
	}
	return c.ProcessState.ExitCode()
}
