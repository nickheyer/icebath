//go:build windows

package proc

import (
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func group(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

func kill(cmd *exec.Cmd) error {
	err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	if err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

func killPID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 137)
}

// Process table read from a toolhelp snapshot
func snapshot() (Table, error) {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(h, &e); err != nil {
		return nil, err
	}
	var t Table
	for {
		t = append(t, Process{PID: int(e.ProcessID), PPID: int(e.ParentProcessID)})
		if err := windows.Process32Next(h, &e); err != nil {
			break
		}
	}
	return t, nil
}
