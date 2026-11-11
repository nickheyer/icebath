//go:build !windows

package tuiserver

import (
	"os/exec"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// No console window exists to hide here
func hideSlashWorkerWindow(cmd *exec.Cmd) { _ = cmd }

// Terminates then kills the worker's whole process group while its leader lives, releasing the group once empty
func stopWorkerGroup(w *slashWorker) {
	pgid := w.cmd.Process.Pid
	if !w.poll() {
		for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
			_ = syscall.Kill(-pgid, sig)
			w.wait(time.Second)
		}
	}
	tools.ReleaseProcessGroupOnceEmpty(pgid, func() { w.poll() }, time.Second)
}
