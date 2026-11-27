//go:build !linux

package git

import "os/exec"

// A run outlives this process on this system until it finishes
func owned(*exec.Cmd) {}
