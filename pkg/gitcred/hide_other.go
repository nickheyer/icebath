//go:build !windows

package gitcred

import "os/exec"

// Leaves the helper untouched where no console window exists
func hideWindow(*exec.Cmd) {}
