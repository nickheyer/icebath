//go:build !windows

package hostinfo

import "os/exec"

// Programs open no console window outside Windows
func hideConsole(*exec.Cmd) {}
