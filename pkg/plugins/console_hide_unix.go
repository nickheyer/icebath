//go:build !windows

package plugins

import "os/exec"

// POSIX children have no console window to hide, the creation flags staying zero
func hideConsoleWindow(*exec.Cmd) {}
