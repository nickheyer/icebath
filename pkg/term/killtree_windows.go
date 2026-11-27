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

// Live processes of this machine as pid to parent pid, from a toolhelp snapshot; none when it can't be taken
func Processes() map[int]int {
	table := map[int]int{}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return table
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		table[int(entry.ProcessID)] = int(entry.ParentProcessID)
	}
	return table
}
