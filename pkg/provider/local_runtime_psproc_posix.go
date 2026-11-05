//go:build !windows

package provider

import (
	"errors"
	"syscall"
	"time"
)

// kill(pid, 0) reachability where EPERM still proves the pid exists
func psKillZero(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// SIGTERM or SIGKILL mapped to psutil NoSuchProcess and AccessDenied
func psSignal(pid int, kill bool) error {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	err := syscall.Kill(pid, sig)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ESRCH):
		return &psError{Class: "NoSuchProcess", Pid: pid}
	case errors.Is(err, syscall.EPERM):
		return &psError{Class: "AccessDenied", Pid: pid}
	}
	return err
}

// Reaps an own child or polls a foreign pid with growing sleeps until it is gone
func psWait(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	interval := 100 * time.Microsecond
	for {
		var status syscall.WaitStatus
		got, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		switch {
		case err == nil && got == pid:
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ECHILD) || err != nil:
			if !psPidExists(pid) {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			return psTimeout(pid, timeout)
		}
		time.Sleep(min(interval, time.Until(deadline)))
		interval = min(interval*2, 40*time.Millisecond)
	}
}
