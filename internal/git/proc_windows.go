package git

import (
	"os/exec"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/proc"
)

// Runs without a console window of its own
func group(cmd *exec.Cmd) {
	proc.HideWindow(cmd)
}

// Program and arguments before git's own of a run that yields to every other process of the machine
func nicely() (string, []string) { return "git", nil }

// Starts the run below the normal priority, so it yields to every other process of the machine
func lower(cmd *exec.Cmd) {
	cmd.SysProcAttr.CreationFlags |= windows.BELOW_NORMAL_PRIORITY_CLASS
}
