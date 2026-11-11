//go:build windows

package selfupdate

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Suspended windowless completion child, bound to the update job before it runs
func completionChildAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_SUSPENDED}
}

// Ends the completion child and its descendants through taskkill, failing when taskkill does
func killCompletionTree(process *os.Process) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(process.Pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run()
}
