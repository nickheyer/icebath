//go:build windows

package pywebbrowser

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
)

// Process attributes of a browser start, Windows has no sessions to detach into
var sessionAttr *syscall.SysProcAttr

// Opens a url through its shell association the way os.startfile does
func startfile(url string) error {
	target, err := windows.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, nil, target, nil, nil, windows.SW_SHOWNORMAL)
}

// Controller of the default Windows browser, built fresh for each open
func newWindowsDefault() *browser {
	return &browser{kind: kindPlatform, args: []string{"%s"},
		launch: func(_ *browser, url string) (bool, error) { return startfile(url) == nil, nil }}
}

// Value of an environment variable or the fallback when it is unset
func environGet(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

// Default browser first, then common browsers on the path, then both Edge install locations
func registerPlatformBrowsers() {
	register("windows-default", newWindowsDefault, nil, false)
	edge64 := pypath.NtJoin(environGet("PROGRAMFILES(x86)", `C:\Program Files (x86)`), `Microsoft\Edge\Application\msedge.exe`)
	edge32 := pypath.NtJoin(environGet("PROGRAMFILES", `C:\Program Files`), `Microsoft\Edge\Application\msedge.exe`)
	for _, name := range []string{"firefox", "seamonkey", "mozilla", "chrome", "opera", edge64, edge32} {
		if _, ok := pyshutil.Which(name, nil); ok {
			register(name, nil, newBackground(name), false)
		}
	}
	if _, ok := pyshutil.Which("MicrosoftEdge.exe", nil); ok {
		register("microsoft-edge", nil, newChrome("MicrosoftEdge.exe"), false)
	}
}
