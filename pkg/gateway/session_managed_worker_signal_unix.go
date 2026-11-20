//go:build !windows

package gateway

import (
	"errors"
	"os"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Own session for the child so it leads a process group a close can signal whole
func managedWorkerAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// Terminates or kills the unreaped child through its handle, a reaped one being left alone
func (p *workerProcess) signal(kill bool) error {
	if p.poll() {
		return nil
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	if err := p.cmd.Process.Signal(sig); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// Signals a verified interpreter by pid, terminate or kill
func signalVerifiedWorker(pid int, kill bool) error {
	if kill {
		return proc.KillPID(pid)
	}
	return proc.Terminate(pid)
}

// Signals the worker's whole process group while it is unreaped and leads one, otherwise the handle alone
func (w *managedWorker) signalGroup(kill bool) error {
	if !w.process.poll() {
		pgid, err := syscall.Getpgid(w.process.Pid)
		if err == nil && pgid == w.process.Pid {
			sig := syscall.SIGTERM
			if kill {
				sig = syscall.SIGKILL
			}
			err = syscall.Kill(-w.process.Pid, sig)
			if err == nil {
				return nil
			}
		}
		if err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
			return err
		}
	}
	return w.process.signal(kill)
}
