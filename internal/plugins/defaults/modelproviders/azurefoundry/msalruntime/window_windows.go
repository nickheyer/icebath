//go:build windows

package msalruntime

import "golang.org/x/sys/windows"

var (
	procGetConsoleWindow = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
	procGetAncestor      = windows.NewLazySystemDLL("user32.dll").NewProc("GetAncestor")
)

const gaRootOwner = 3

// Root owner of the console window this process runs in, zero for a process without a console
func ConsoleWindow() WindowHandle {
	console, _, _ := procGetConsoleWindow.Call()
	if console == 0 {
		return 0
	}
	root, _, _ := procGetAncestor.Call(console, gaRootOwner)
	return WindowHandle(root)
}

// Desktop window, the parent of last resort for broker UI
func DesktopWindow() WindowHandle { return WindowHandle(windows.GetDesktopWindow()) }
