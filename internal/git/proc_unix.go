//go:build !windows

package git

import (
	"os/exec"
	"syscall"
)

// Starts the run in a process group of its own, so stopping it stops every process it started
func group(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

// Program and arguments before git's own of a run that yields to every other process of the machine
func nicely() (string, []string) { return "nice", []string{"-n", "19", "git"} }

// Lowers the priority of a run, which nice already did
func lower(*exec.Cmd) {}
