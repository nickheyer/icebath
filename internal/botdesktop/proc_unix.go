//go:build !windows

package botdesktop

import (
	"os"
	"syscall"
)

func pgid(pid int) (int, error) { return syscall.Getpgid(pid) }

// Reaps the process without waiting when it is a child of this one
func reap(pid int) {
	var status syscall.WaitStatus
	syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
}

// Starts a session of its own unless this process already leads its group
func detach() {
	if syscall.Getpgrp() != os.Getpid() {
		syscall.Setsid()
	}
}

func sessionAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

const sigterm = syscall.SIGTERM

func root() bool { return os.Geteuid() == 0 }
