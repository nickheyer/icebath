//go:build !windows

package connections

import "os/exec"

// Children start without a window on hosts that have no console windows
func hideChildWindow(*exec.Cmd) {}
