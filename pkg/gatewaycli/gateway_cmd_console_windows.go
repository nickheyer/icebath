//go:build windows

package gatewaycli

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows"
)

var (
	procGetConsoleWindow      = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
	procSetConsoleCtrlHandler = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")
)

// Whether Windows assigned this process a console window, nil when the call is unavailable
func windowsConsoleWindowAttached() *bool {
	if err := procGetConsoleWindow.Find(); err != nil {
		return nil
	}
	hwnd, _, _ := procGetConsoleWindow.Call()
	attached := hwnd != 0
	return &attached
}

// Makes a detached Windows gateway ignore console control broadcasts from sibling CLIs
func absorbWindowsConsoleControls() {
	signal.Ignore(os.Interrupt, syscall.SIGINT)
	if err := procSetConsoleCtrlHandler.Find(); err == nil {
		_, _, _ = procSetConsoleCtrlHandler.Call(0, 1)
	}
}
