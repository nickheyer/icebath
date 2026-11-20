//go:build windows

package gateway

import (
	"errors"
	"os"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Windows children take no session flag, the worker exits itself when its owner dies
func managedWorkerAttr() *syscall.SysProcAttr { return nil }

// Terminates the unreaped child through its handle, a reaped one being left alone
func (p *workerProcess) signal(kill bool) error {
	if p.poll() {
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// Terminates a verified interpreter by pid
func signalVerifiedWorker(pid int, kill bool) error { return proc.KillPID(pid) }

// Windows has no process groups to signal, so the handle alone
func (w *managedWorker) signalGroup(kill bool) error { return w.process.signal(kill) }
