//go:build !windows

package winenv

import "os/exec"

// Leaves the child as is where no console window exists
func hideWindow(*exec.Cmd) {}
