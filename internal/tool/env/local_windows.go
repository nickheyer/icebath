package env

import (
	"io/fs"
	"os/exec"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

func startGroup(cmd *exec.Cmd) {
	proc.Hide(cmd)
}

func multiLinked(fs.FileInfo) bool {
	return false
}

func (p *cmdProcess) adopt() {
	pid := p.cmd.Process.Pid
	p.pgid = pid
	if id, err := proc.Identify(pid); err == nil {
		p.leader, p.known = id, true
	}
}

func killGroup(p *cmdProcess) {
	killed := false
	if p.known {
		killed = proc.SignalTree(p.leader, syscall.SIGKILL) == nil
	}
	if !killed {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
}

func forceKillGroup(p *cmdProcess) {
	killGroup(p)
}
