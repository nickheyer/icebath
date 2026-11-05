//go:build !windows

package mcp

import "golang.org/x/sys/unix"

// Process groups reach reparented grandchildren on POSIX
const groupSignalSupported = true

// Signal number of a termination pass
func signalNumber(force bool) int {
	if force {
		return int(unix.SIGKILL)
	}
	return int(unix.SIGTERM)
}

// Our own process group, never signalled
func ownProcessGroup() (int, bool) { return unix.Getpgrp(), true }

// Sends a signal to a whole process group, zero probing for a live member
func signalGroup(pgid, sig int) error { return unix.Kill(-pgid, unix.Signal(sig)) }

// Sends a signal to one pid, ignoring a vanished or foreign process
func signalProcessTree(pid, sig int) { _ = unix.Kill(pid, unix.Signal(sig)) }
