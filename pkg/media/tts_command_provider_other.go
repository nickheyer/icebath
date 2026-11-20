//go:build !windows

package media

import "os/exec"

// Win32 creation flags have no POSIX counterpart, the group comes from SpawnOwned
func applyCreationFlags(*exec.Cmd, uint32) {}

// POSIX helpers have no console window to hide
func hideConsoleWindow(*exec.Cmd) {}
