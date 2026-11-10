//go:build !windows

package gateway

import "os/exec"

// Leaves the command as is, only Windows consoles flash a window
func hideConsole(*exec.Cmd) {}
