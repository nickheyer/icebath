//go:build !windows

package recipes

import (
	"os/exec"
	"runtime"
)

// Shell command of a recipe line
func shellCommand(command string) *exec.Cmd {
	sh := "/bin/sh"
	if runtime.GOOS == "android" {
		sh = "/system/bin/sh"
	}
	return exec.Command(sh, "-c", command)
}
