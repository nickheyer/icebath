//go:build !unix && !windows

package executil

import (
	"os/exec"
	"runtime"
)

// Path of the platform shell, rc on Plan 9 and none elsewhere
func Shell() (string, error) {
	if runtime.GOOS == "plan9" {
		return "/bin/rc", nil
	}
	return "", ErrNoShell
}

// Command that runs a command line through the platform shell as shell -c command
func ShellCommand(command string) (*exec.Cmd, error) {
	shell, err := Shell()
	if err != nil {
		return nil, err
	}
	return exec.Command(shell, "-c", command), nil
}
