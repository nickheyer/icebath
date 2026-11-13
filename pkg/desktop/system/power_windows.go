//go:build windows

package system

import (
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmPowerBroadcast       = 0x0218
	wmWtsSessionChange     = 0x02B1
	pbtAPMSuspend          = 0x4
	pbtAPMResumeAutomatic  = 0x12
	pbtAPMPowerStatusChang = 0xA
	wtsSessionUnlock       = 0x8
	notifyForThisSession   = 0
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	wtsapi32                = windows.NewLazySystemDLL("wtsapi32.dll")
	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procGetLastInputInfo    = user32.NewProc("GetLastInputInfo")
	procWTSRegisterSession  = wtsapi32.NewProc("WTSRegisterSessionNotification")
	procGetSystemPowerState = kernel32.NewProc("GetSystemPowerStatus")
	procGetTickCount        = kernel32.NewProc("GetTickCount")
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type winMsg struct {
	Hwnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// WindowsPower receives WM_POWERBROADCAST and session unlock notifications on a hidden top level window
type WindowsPower struct{}

// Power monitor over a hidden window message loop
func NewPowerMonitor() *WindowsPower { return &WindowsPower{} }

// Creates the hidden window on its own locked thread and pumps its messages
func (p *WindowsPower) Start(ev PowerEvents) error {
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		className, _ := windows.UTF16PtrFromString("IcebathPowerMonitor")
		proc := windows.NewCallback(func(hwnd windows.HWND, msg uint32, wparam, lparam uintptr) uintptr {
			switch msg {
			case wmPowerBroadcast:
				switch wparam {
				case pbtAPMSuspend:
					ev.Suspend()
				case pbtAPMResumeAutomatic:
					ev.Resume()
				case pbtAPMPowerStatusChang:
					if p.IsOnBatteryPower() {
						ev.OnBattery()
					} else {
						ev.OnAC()
					}
				}
				return 1
			case wmWtsSessionChange:
				if wparam == wtsSessionUnlock {
					ev.UnlockScreen()
				}
				return 0
			}
			r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
			return r
		})
		var instance windows.Handle
		windows.GetModuleHandleEx(0, nil, &instance)
		wc := wndClassEx{WndProc: proc, Instance: instance, ClassName: className}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
			ready <- err
			return
		}
		hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, 0, 0, uintptr(instance), 0)
		if hwnd == 0 {
			ready <- err
			return
		}
		procWTSRegisterSession.Call(hwnd, notifyForThisSession)
		ready <- nil
		var msg winMsg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}()
	if err := <-ready; err != nil {
		return errors.New("power monitor window: " + err.Error())
	}
	return nil
}

// True while the AC line is offline
func (p *WindowsPower) IsOnBatteryPower() bool {
	var status systemPowerStatus
	if r, _, _ := procGetSystemPowerState.Call(uintptr(unsafe.Pointer(&status))); r == 0 {
		return false
	}
	return status.ACLineStatus == 0
}

// Seconds since the last input event of the session
func (p *WindowsPower) SystemIdleTime() int {
	info := struct {
		Size uint32
		Time uint32
	}{Size: 8}
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0
	}
	now, _, _ := procGetTickCount.Call()
	return int((uint32(now) - info.Time) / 1000)
}
