//go:build !windows

package proc

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// Process group pid belongs to
func GroupOf(pid int) (int, error) {
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		return 0, signalError(pid, err)
	}
	return pgid, nil
}

// Session pid belongs to
func SessionOf(pid int) (int, error) {
	sid, err := unix.Getsid(pid)
	if err != nil {
		return 0, signalError(pid, err)
	}
	return sid, nil
}

// Whether a group number may be signalled at all, the init group and this process's own group never being one
func signallableGroup(pgid int) error {
	if pgid <= 1 || pgid == unix.Getpgrp() {
		return fmt.Errorf("process group %d is not a group this process may signal", pgid)
	}
	return nil
}

// Delivers sig to every member of the process group leader leads, ErrReused or ErrNotFound once the leader is gone
func SignalGroup(leader Identity, sig syscall.Signal) error {
	if err := signallableGroup(leader.PID); err != nil {
		return err
	}
	return signalLedGroup(leader, sig)
}

// Signals the group by number once id is seen leading it, the live leader keeping the number from reuse
func checkedGroupSignal(id Identity, sig syscall.Signal) error {
	start, err := Birth(id.PID)
	if err != nil {
		return err
	}
	if start != id.Start {
		return fmt.Errorf("pid %d: %w", id.PID, ErrReused)
	}
	return killLedGroup(id.PID, sig)
}

// Signals group pgid after confirming its own number still leads it
func killLedGroup(pgid int, sig syscall.Signal) error {
	if led, err := unix.Getpgid(pgid); err != nil {
		return signalError(pgid, err)
	} else if led != pgid {
		return fmt.Errorf("pid %d no longer leads its process group", pgid)
	}
	return groupError(pgid, unix.Kill(-pgid, sig))
}

// Maps a group kill failure to ErrNotFound or ErrAccessDenied
func groupError(pgid int, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ESRCH):
		return fmt.Errorf("process group %d: %w", pgid, ErrNotFound)
	case errors.Is(err, unix.EPERM):
		return fmt.Errorf("process group %d: %w: %w", pgid, ErrAccessDenied, err)
	}
	return fmt.Errorf("process group %d: %w", pgid, err)
}

// Whether process group pgid still has a member, a refused probe counting as present
func GroupExists(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := unix.Kill(-pgid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}
