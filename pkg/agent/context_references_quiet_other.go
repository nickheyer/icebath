//go:build !windows

package agent

import "os/exec"

// Console windows exist only on Windows, so there is nothing to hide elsewhere
func hideConsoleWindow(*exec.Cmd) {}
