//go:build windows

package tools

import (
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

func executable(string) bool { return true }

// Hands the kernel an inheritable SYNCHRONIZE handle to icebath, which its watchdog waits on to exit with it; the release closes icebath's
// copy once the kernel started
func parentHandle(env map[string]string, cmd *exec.Cmd) func() {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, true, windows.GetCurrentProcessId())
	if err != nil || h == 0 {
		delete(env, "ICEBATH_KERNEL_PARENT_PROCESS_HANDLE")
		return func() {}
	}
	env["ICEBATH_KERNEL_PARENT_PROCESS_HANDLE"] = strconv.FormatUint(uint64(h), 10)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, syscall.Handle(h))
	return func() { windows.CloseHandle(h) }
}
