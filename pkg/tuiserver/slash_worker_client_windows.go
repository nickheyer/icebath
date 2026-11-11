//go:build windows

package tuiserver

import (
	"os/exec"
	"syscall"
	"time"
)

// Starts the worker without a console window
func hideSlashWorkerWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x08000000
}

// Terminates then kills the worker, each step given a second to exit
func stopWorkerGroup(w *slashWorker) {
	for range 2 {
		if w.poll() {
			return
		}
		suppress(func() {
			_ = w.cmd.Process.Kill()
			w.wait(time.Second)
		})
	}
}
