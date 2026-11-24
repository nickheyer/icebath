//go:build unix

package executil

import (
	"os/exec"
	"runtime"
)

// Path of the POSIX shell, /system/bin/sh on Android and /bin/sh elsewhere
func Shell() (string, error) { return posixShell(), nil }

// Command that runs a command line through the POSIX shell as sh -c command
func ShellCommand(command string) (*exec.Cmd, error) {
	return exec.Command(posixShell(), "-c", command), nil
}

func posixShell() string {
	if runtime.GOOS == "android" {
		return "/system/bin/sh"
	}
	return "/bin/sh"
}
