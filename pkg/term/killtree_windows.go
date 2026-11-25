//go:build windows

package term

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

func killPID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 137)
}

// Process table read from a toolhelp snapshot
func processTable() ([]procRow, error) {
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
	var t []procRow
	for {
		t = append(t, procRow{pid: int(e.ProcessID), ppid: int(e.ParentProcessID)})
		if err := windows.Process32Next(h, &e); err != nil {
			break
		}
	}
	return t, nil
}
