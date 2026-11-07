//go:build !windows

package botdesktop

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// True when the path is executable by this process
func accessExec(path string) bool { return unix.Access(path, unix.X_OK) == nil }

// Exclusive flock on path opened for append, released by the returned func
func flockPath(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func getpgid(pid int) (int, error) { return syscall.Getpgid(pid) }

func killpg(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }

func killPid(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// Reaps pid without blocking when it is a child of this process
func waitpidNoHang(pid int) {
	var ws syscall.WaitStatus
	syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
}

// Starts a new session unless this process already leads its group
func detachSession() {
	if syscall.Getpgrp() != os.Getpid() {
		syscall.Setsid()
	}
}

// Puts the launcher in a session of its own
func newSessionAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

func isESRCH(err error) bool { return err == syscall.ESRCH }

func isEPERM(err error) bool { return err == syscall.EPERM }
