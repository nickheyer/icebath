//go:build !windows

package pysubprocess

import (
	"os/exec"
	"runtime"
)

// Command Popen runs for shell=True, the string handed to /bin/sh -c or the Android system shell
func ShellCommand(command string) (*exec.Cmd, error) {
	sh := "/bin/sh"
	if runtime.GOOS == "android" {
		sh = "/system/bin/sh"
	}
	return exec.Command(sh, "-c", command), nil
}
