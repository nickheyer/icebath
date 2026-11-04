//go:build windows

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows half of the registry plumbing, taskkill tree kills and host memory

// The systemd user bus never exists on Windows
func secureUserRuntimeDir(path string) bool { return false }

func ownedUnixSocket(path string) bool { return false }

// Kills pid and its tree through taskkill, falling back to a direct terminate
func terminateHostPidWindows(pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	hideConsoleWindow(cmd)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			if p, perr := os.FindProcess(pid); perr == nil {
				p.Kill()
			}
		}
	}
}

var procCreatePseudoConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreatePseudoConsole")

// ConPTY exists from Windows 10 1809 on
func ptyAvailable() bool { return procCreatePseudoConsole.Find() == nil }

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

// Physical memory of the host in bytes
func physicalMemoryBytes() (int64, bool) {
	var st memoryStatusEx
	st.Length = uint32(unsafe.Sizeof(st))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if r == 0 {
		return 0, false
	}
	return int64(st.TotalPhys), true
}
