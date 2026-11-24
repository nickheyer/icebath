//go:build unix

package executil

import (
	"os/exec"
	"syscall"
)

// Kills a started command, its whole process group when its attributes made it the group leader: Setsid, or Setpgid or Foreground with Pgid 0
func kill(cmd *exec.Cmd) error {
	if a := cmd.SysProcAttr; a != nil && (a.Setsid || (a.Setpgid || a.Foreground) && a.Pgid == 0) {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
			return nil
		}
	}
	return cmd.Process.Kill()
}
