//go:build !windows

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Maps a kill failure for pid to ErrNotFound or ErrAccessDenied
func signalError(pid int, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ESRCH):
		return fmt.Errorf("pid %d: %w", pid, ErrNotFound)
	case errors.Is(err, unix.EPERM):
		return denied(pid, err)
	}
	return fmt.Errorf("pid %d: %w", pid, err)
}

// Whether a signal zero test finds pid, a refused test meaning another user owns a live process
func probe(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// Whether pid names a running process, without signalling it and with zombies counting as dead
func Alive(pid int) bool {
	if pid <= 0 || !probe(pid) {
		return false
	}
	st, err := StatusOf(pid)
	switch {
	case err == nil:
		return !st.Exited() && groupLeader(pid)
	case errors.Is(err, ErrNotFound):
		return probe(pid)
	}
	return true
}

// Whether pid is listed as a process at all, an exited child its parent has not collected included
func Exists(pid int) bool {
	switch {
	case pid < 0:
		return false
	case pid == 0:
		return zeroExists()
	}
	return probe(pid) && groupLeader(pid)
}

// Delivers sig to the single process pid, for callers that verified its identity first
func signalPID(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("pid %d does not name a single process", pid)
	}
	return signalError(pid, unix.Kill(pid, sig))
}

// Signals the pid after a fresh start stamp read shows it still names id
func checkedSignal(id Identity, sig syscall.Signal) error {
	start, err := Birth(id.PID)
	if err != nil {
		return err
	}
	if start != id.Start {
		return fmt.Errorf("pid %d: %w", id.PID, ErrReused)
	}
	return signalPID(id.PID, sig)
}

// Ends this process by sig with its default action so a supervisor sees the signal in the exit status
func ExitBySignal(sig syscall.Signal) {
	signal.Reset(sig)
	if unix.Kill(os.Getpid(), sig) == nil {
		time.Sleep(2 * time.Second)
	}
	os.Exit(128 + int(sig))
}
