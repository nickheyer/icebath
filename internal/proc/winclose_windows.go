package proc

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procPostMessage = windows.NewLazySystemDLL("user32.dll").NewProc("PostMessageW")

// WM_CLOSE, the request taskkill without /F sends to each top level window
const wmClose = 0x0010

// Top level windows found by one enumeration, keyed by the pid owning each
type windowOwners map[uint32][]windows.HWND

// EnumWindows callback filling the windowOwners its parameter points at, created once since callbacks are never freed
var collectWindows = sync.OnceValue(func() uintptr {
	return windows.NewCallback(func(hwnd windows.HWND, param uintptr) uintptr {
		owners := *(*windowOwners)(unsafe.Pointer(&param))
		var pid uint32
		if _, err := windows.GetWindowThreadProcessId(hwnd, &pid); err == nil && pid != 0 {
			owners[pid] = append(owners[pid], hwnd)
		}
		return 1
	})
})

// Posts WM_CLOSE to every top level window of the processes behind handles, true when any window received it
func closeWindows(handles []windows.Handle) bool {
	owners := windowOwners{}
	if windows.EnumWindows(collectWindows(), *(*unsafe.Pointer)(unsafe.Pointer(&owners))) != nil {
		return false
	}
	posted := false
	for _, h := range handles {
		pid, err := windows.GetProcessId(h)
		if err != nil || handleExited(h) {
			continue
		}
		for _, hwnd := range owners[pid] {
			if r, _, _ := procPostMessage.Call(uintptr(hwnd), wmClose, 0, 0); r != 0 {
				posted = true
			}
		}
	}
	return posted
}
