//go:build !windows

package provider

import (
	"os/exec"
)

// GlobalMemoryStatusEx exists only on Windows
func windowsMemoryStatus() *windowsMemoryStatusEx { return nil }

// Console windows exist only on Windows
func hideChildWindow(*exec.Cmd) {}
