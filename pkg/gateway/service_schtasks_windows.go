//go:build windows

package gateway

import (
	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/tools"
)

var shell32ShellExecuteW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteW")

// Runs schtasks.exe hidden under its fifteen second timeout
func execSchtasksOnHost(args []string) (int, string, string, error) {
	if err := assertWindows(); err != nil {
		return 0, "", "", err
	}
	code, out, errText := tools.ExecSchtasks(args)
	return code, out, errText, nil
}

// Whether GetConsoleMode accepts the stdin handle, nil off Windows
func stdinConsoleModeOK() *bool {
	if serviceGOOS != "windows" {
		return nil
	}
	var mode uint32
	h, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	ok := err == nil && windows.GetConsoleMode(h, &mode) == nil
	return &ok
}

// ShellExecuteW runas of file with params in cwd hidden, its raw result code
func shellExecuteRunas(file, params, cwd string) (uintptr, error) {
	if err := shell32ShellExecuteW.Find(); err != nil {
		return 0, err
	}
	ptrs := make([]*uint16, 4)
	for i, s := range []string{"runas", file, params, cwd} {
		p, err := windows.UTF16PtrFromString(s)
		if err != nil {
			return 0, err
		}
		ptrs[i] = p
	}
	r, _, _ := shell32ShellExecuteW.Call(0, uintptrOf(ptrs[0]), uintptrOf(ptrs[1]), uintptrOf(ptrs[2]), uintptrOf(ptrs[3]), 0)
	return r, nil
}
