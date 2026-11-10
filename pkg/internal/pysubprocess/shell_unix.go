//go:build !windows

package pysubprocess

import (
	"os/exec"
	"runtime"
)

// Shell Popen runs for shell=True, the Android system shell on Android
func UnixShell() string {
	if runtime.GOOS == "android" {
		return "/system/bin/sh"
	}
	return "/bin/sh"
}

// Command Popen runs for shell=True, the string handed to /bin/sh -c or the Android system shell
func ShellCommand(command string) (*exec.Cmd, error) {
	return exec.Command(UnixShell(), "-c", command), nil
}
