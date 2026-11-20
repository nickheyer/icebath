//go:build windows

package codeexec

import (
	"os"
	"os/exec"
)

// Windows children carry no signal, the raw exit status stands as the returncode
func signalNumber(e *exec.ExitError) int { return -e.ExitCode() }

// TerminateProcess like Popen.terminate on Windows
func terminateProcess(p *os.Process) { _ = p.Kill() }
