//go:build windows

package provider

import (
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The System Idle Process holds pid 0 on Windows
const psLowestPID = 0

// Parent pid from a toolhelp process snapshot
func psPPID(pid int) (int, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if int(entry.ProcessID) == pid {
			return int(entry.ParentProcessID), nil
		}
	}
	return 0, &psError{Class: "NoSuchProcess", Pid: pid}
}

// Process still active, an access denied open proves it exists
func psPidExists(pid int) bool {
	if pid < 0 {
		return false
	}
	if pid == 0 {
		return true
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == 259
}

// TerminateProcess with exit code 15 for both terminate and kill like psutil
func psSignal(pid int, _ bool) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return &psError{Class: "AccessDenied", Pid: pid}
		}
		return &psError{Class: "NoSuchProcess", Pid: pid}
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 15); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) && !psPidExists(pid) {
			return &psError{Class: "NoSuchProcess", Pid: pid}
		}
		return &psError{Class: "AccessDenied", Pid: pid, Msg: err.Error()}
	}
	return nil
}

// Waits on the process handle, a pid that cannot be opened has already exited
func psWait(pid int, timeout time.Duration) error {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(h)
	event, err := windows.WaitForSingleObject(h, uint32(timeout.Milliseconds()))
	if err != nil {
		return err
	}
	if event == uint32(windows.WAIT_TIMEOUT) {
		return psTimeout(pid, timeout)
	}
	return nil
}
