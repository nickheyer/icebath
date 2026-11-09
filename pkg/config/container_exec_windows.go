//go:build windows

package config

import (
	"errors"
	"os"
	"os/exec"
)

// Runs the container exec command on the inherited stdio and returns its exit code, the Windows execvp
func execContainerCommand(argv []string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
