package git

import (
	"os/exec"
	"syscall"
)

// Kills the run once this process ends, the thread that started it kept alive for its length
func owned(cmd *exec.Cmd) {
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
