//go:build !windows

package install

import "os/exec"

// Leaves the child streams as they are, no console window exists to hide
func hideConsole(cmd *exec.Cmd) {}
