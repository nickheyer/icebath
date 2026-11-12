//go:build windows

package files

import (
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// Creates git with CREATE_NO_WINDOW and a hidden window so no console flashes
func configureGitProcess(cmd *exec.Cmd, plan GitSpawnPlan, _ bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: plan.WindowsHide, CreationFlags: plan.CreationFlags}
}

// Starts a console child without a console window
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: CreateNoWindow}
}

// A root that already exited has no tree left for taskkill to walk
func hasTreeToKill(cmd *exec.Cmd) bool {
	return cmd.Process != nil && cmd.ProcessState == nil
}

func taskkill(args []string) error {
	kill := exec.Command("taskkill", args...)
	hideConsole(kill)
	done := make(chan error, 1)
	if err := kill.Start(); err != nil {
		return err
	}
	go func() { done <- kill.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		kill.Process.Kill()
		return syscall.ETIMEDOUT
	}
}

// taskkill /T /F of the command tree, the root alone when taskkill fails
func killTree(cmd *exec.Cmd) {
	if !hasTreeToKill(cmd) {
		return
	}
	if err := taskkill([]string{"/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F"}); err != nil {
		cmd.Process.Kill()
	}
}

// One taskkill call for every live tree, will quit cannot wait on serial ones
func killTrees(cmds []*exec.Cmd) {
	var args []string
	var live []*exec.Cmd
	for _, cmd := range cmds {
		if hasTreeToKill(cmd) {
			live = append(live, cmd)
			args = append(args, "/PID", strconv.Itoa(cmd.Process.Pid))
		}
	}
	if len(live) == 0 {
		return
	}
	if err := taskkill(append(args, "/T", "/F")); err != nil {
		for _, cmd := range live {
			cmd.Process.Kill()
		}
	}
}
