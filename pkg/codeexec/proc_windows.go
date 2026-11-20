//go:build windows

package codeexec

import (
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// Keeps a console window from flashing for a probe or child process
func hideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

func isExecutable(p string) bool { return true }

// Hands the kernel an inheritable SYNCHRONIZE handle to this process for its parent death watchdog
func parentProcessHandle(childEnv map[string]string, cmd *exec.Cmd) func() {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, true, windows.GetCurrentProcessId())
	if err != nil || h == 0 {
		delete(childEnv, "ICEBATH_KERNEL_PARENT_PROCESS_HANDLE")
		return func() {}
	}
	childEnv["ICEBATH_KERNEL_PARENT_PROCESS_HANDLE"] = strconv.FormatUint(uint64(h), 10)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, syscall.Handle(h))
	return func() { _ = windows.CloseHandle(h) }
}
