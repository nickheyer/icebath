//go:build !windows

package process

import (
	"os/exec"

	"github.com/nickheyer/icebath/internal/proc"
)

func detach(cmd *exec.Cmd) { proc.NewSession(cmd) }
