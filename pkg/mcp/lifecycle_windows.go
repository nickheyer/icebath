//go:build windows

package mcp

import (
	"errors"
	"os/exec"
	"strconv"
)

// Windows has no process groups that reach reparented descendants
const groupSignalSupported = false

// Signal number of a termination pass, SIGKILL 9 or SIGTERM 15
func signalNumber(force bool) int {
	if force {
		return 9
	}
	return 15
}

// Windows exposes no process group of ours
func ownProcessGroup() (int, bool) { return 0, false }

func signalGroup(pgid, sig int) error { return errors.ErrUnsupported }

// Terminates a pid and every descendant still alive, the Windows counterpart of the group kill
func signalProcessTree(pid, sig int) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}
