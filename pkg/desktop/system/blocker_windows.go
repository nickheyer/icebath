//go:build windows

package system

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	powerRequestContextVersion      = 0
	powerRequestContextSimpleString = 0x1
	powerRequestDisplayRequired     = 0
	powerRequestExecutionRequired   = 3
)

var (
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procPowerCreateRequest = kernel32.NewProc("PowerCreateRequest")
	procPowerSetRequest    = kernel32.NewProc("PowerSetRequest")
	procPowerClearRequest  = kernel32.NewProc("PowerClearRequest")
)

// REASON_CONTEXT with the simple string arm of its union, padded to the detailed arm size
type reasonContext struct {
	Version            uint32
	Flags              uint32
	SimpleReasonString *uint16
	_                  [2]uintptr
}

type powerRequestHold struct {
	once    sync.Once
	handle  windows.Handle
	request uintptr
}

func (h *powerRequestHold) Release() {
	h.once.Do(func() {
		procPowerClearRequest.Call(uintptr(h.handle), h.request)
		windows.CloseHandle(h.handle)
	})
}

// Holds a power request, execution required for app suspension and display required otherwise
func platformInhibit(kind KeepAwakeType, reason string) Hold {
	text, err := windows.UTF16PtrFromString(reason)
	if err != nil {
		return nil
	}
	ctx := reasonContext{Version: powerRequestContextVersion, Flags: powerRequestContextSimpleString, SimpleReasonString: text}
	r, _, _ := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&ctx)))
	handle := windows.Handle(r)
	if handle == windows.InvalidHandle || handle == 0 {
		return nil
	}
	request := uintptr(powerRequestExecutionRequired)
	if kind == PreventDisplaySleep {
		request = powerRequestDisplayRequired
	}
	if ok, _, _ := procPowerSetRequest.Call(uintptr(handle), request); ok == 0 {
		windows.CloseHandle(handle)
		return nil
	}
	return &powerRequestHold{handle: handle, request: request}
}
