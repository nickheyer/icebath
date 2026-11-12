//go:build !windows

package proc

import (
	"errors"
	"slices"
	"syscall"
)

// Whether pid names a process as psutil pid_exists answers, zombies counted and the error psutil raises returned
func PidExists(pid int) (bool, error) {
	if pid < 0 {
		return false, nil
	}
	if pid == 0 {
		return pidListed(0)
	}
	live, err := posixPidExists(pid)
	if err != nil || !live {
		return live, err
	}
	return threadGroupLeader(pid)
}

// Signal zero probe of psutil psposix pid_exists, a denied signal meaning a live process
func posixPidExists(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	}
	return false, err
}

// Whether pid appears in the process listing psutil pids reads
func pidListed(pid int) (bool, error) {
	pids, err := PIDs()
	if err != nil {
		return false, err
	}
	return slices.Contains(pids, pid), nil
}

// Whether a signal zero to pid succeeds or is denied, any other failure reading as dead
func SignalProbeLive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
