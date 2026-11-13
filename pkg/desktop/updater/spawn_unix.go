//go:build !windows

package updater

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Starts the helper as its own process group leader
func detachHelper(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Kills the helper's whole process group
func killHelper(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// Signal name that ended a process as Node reports it, empty for a normal exit
func exitSignalName(state *os.ProcessState) string {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return unix.SignalName(status.Signal())
	}
	return ""
}

// Detaches a spawned updater into its own session so it outlives this process
func detachUpdater(cmd *exec.Cmd, detached bool) {
	if detached {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
}

// POSIX children run without a console window to hide
func hideConsole(*exec.Cmd) {}

// Kills the hand-off script's process group, the lone pid when it leads none
func killHandoffTree(child *UpdaterChild, _ bool, _ int) {
	pid := child.PID()
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// Kernel release the minimum system version of a feed is compared with, as os.release reports it
func kernelRelease() string {
	var name unix.Utsname
	if unix.Uname(&name) != nil {
		return ""
	}
	return unix.ByteSliceToString(name.Release[:])
}
