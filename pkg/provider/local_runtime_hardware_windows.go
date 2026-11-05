package provider

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// GlobalMemoryStatusEx counters, nil when the call fails
func windowsMemoryStatus() *windowsMemoryStatusEx {
	if procGlobalMemoryStatusEx.Find() != nil {
		return nil
	}
	var stat memoryStatusEx
	stat.Length = uint32(unsafe.Sizeof(stat))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&stat))); r == 0 {
		return nil
	}
	return &windowsMemoryStatusEx{TotalPhys: stat.TotalPhys, AvailPhys: stat.AvailPhys,
		TotalPageFile: stat.TotalPageFile, AvailPageFile: stat.AvailPageFile}
}

// Starts cmd without a console window as CREATE_NO_WINDOW does
func hideChildWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// Whether a process with pid exists, an access denied open still counting as existing
func processPIDExists(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return true
	}
	return code == 259
}
