//go:build !windows

package mcp

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// setProcessGroup makes the child lead its own session so grandchildren stay reachable
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// processGroup returns the process group of pid, zero when it already exited
func processGroup(pid int) int {
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		return pid
	}
	return pgid
}

// terminateGroup signals the whole group, never our own, falling back to the single pid
func terminateGroup(pid, pgid int, force bool) {
	sig := unix.SIGTERM
	if force {
		sig = unix.SIGKILL
	}
	own, _ := unix.Getpgid(0)
	if pgid > 0 && pgid != own {
		if err := unix.Kill(-pgid, sig); err == nil {
			return
		}
	}
	_ = unix.Kill(pid, sig)
}

// groupAlive reports whether any member of the group still runs
func groupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	own, _ := unix.Getpgid(0)
	if pgid == own {
		return false
	}
	return unix.Kill(-pgid, 0) == nil
}

// signalZero probes a process for existence without affecting it
func signalZero(p *os.Process) bool {
	err := p.Signal(syscall.Signal(0))
	return err == nil || err == unix.EPERM
}

// lockFile takes a non blocking exclusive advisory lock, false when another holder has it
func lockFile(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == unix.EWOULDBLOCK || err == unix.EAGAIN || err == unix.EACCES {
		return false, nil
	}
	return false, err
}

// unlockFile releases an advisory lock
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
