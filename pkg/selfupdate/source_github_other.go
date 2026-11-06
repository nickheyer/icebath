//go:build !windows

package selfupdate

import "os/exec"

// Leaves a POSIX git probe as it is, a console window exists only on Windows
func hideProbeWindow(cmd *exec.Cmd) { _ = cmd }
