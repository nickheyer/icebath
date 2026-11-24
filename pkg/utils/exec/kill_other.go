//go:build !unix && !windows

package executil

import "os/exec"

func kill(cmd *exec.Cmd) error { return cmd.Process.Kill() }
