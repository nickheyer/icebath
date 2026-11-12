//go:build windows

package main

import (
	"sync"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/sys/windows"
)

var (
	user32                         = windows.NewLazySystemDLL("user32.dll")
	dwmapi                         = windows.NewLazySystemDLL("dwmapi.dll")
	procShowWindow                 = user32.NewProc("ShowWindow")
	procGetWindowLongPtrW          = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW          = user32.NewProc("SetWindowLongPtrW")
	procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	procDwmSetWindowAttribute      = dwmapi.NewProc("DwmSetWindowAttribute")
	winTheme                       struct {
		sync.Mutex
		mode string
	}
)

const (
	swShowNoActivate        = 4
	gwlExStyle              = ^uintptr(19)
	wsExLayered             = 0x00080000
	lwaAlpha                = 0x2
	dwmwaUseImmersiveDark   = 20
	dwmwaSystemBackdropType = 38
	dwmsbtNone              = 1
	dwmsbtMainWindow        = 2
	dwmsbtTransientWindow   = 3
	dwmsbtTabbedWindow      = 4
)

func hwndOf(w *application.WebviewWindow) uintptr { return uintptr(w.NativeWindow()) }

func nativeShowInactive(w *application.WebviewWindow) {
	application.InvokeSync(func() { procShowWindow.Call(hwndOf(w), swShowNoActivate) })
}

func nativeSetOpacity(w *application.WebviewWindow, o float64) {
	application.InvokeSync(func() {
		h := hwndOf(w)
		ex, _, _ := procGetWindowLongPtrW.Call(h, gwlExStyle)
		if ex&wsExLayered == 0 {
			procSetWindowLongPtrW.Call(h, gwlExStyle, ex|wsExLayered)
		}
		procSetLayeredWindowAttributes.Call(h, 0, uintptr(uint8(o*255+0.5)), lwaAlpha)
	})
}

func nativeSetVibrancy(*application.WebviewWindow, string) {
	panic("vibrancy is a macOS window material and is never applied on Windows")
}

func nativeSetTrafficLightInset(*application.WebviewWindow, float64) {
	panic("traffic light insets exist only on macOS")
}

func dwmSet(h uintptr, attr uint32, value uint32) {
	procDwmSetWindowAttribute.Call(h, uintptr(attr), uintptr(unsafe.Pointer(&value)), unsafe.Sizeof(value))
}

func nativeSetBackgroundMaterial(w *application.WebviewWindow, material string) {
	kind := uint32(dwmsbtNone)
	switch material {
	case "acrylic":
		kind = dwmsbtTransientWindow
	case "tabbed":
		kind = dwmsbtTabbedWindow
	case "mica":
		kind = dwmsbtMainWindow
	}
	application.InvokeSync(func() { dwmSet(hwndOf(w), dwmwaSystemBackdropType, kind) })
}

func nativeThemeSource() string {
	winTheme.Lock()
	defer winTheme.Unlock()
	if winTheme.mode == "" {
		return "system"
	}
	return winTheme.mode
}

// nativeSetThemeSource records the mode, the shell applies it to every window frame
func nativeSetThemeSource(mode string) {
	winTheme.Lock()
	winTheme.mode = mode
	winTheme.Unlock()
}

func nativeThemeStarted() {}

// nativeApplyWindowTheme paints the window frame dark or light for the theme source
func nativeApplyWindowTheme(w *application.WebviewWindow, dark bool) {
	v := uint32(0)
	if dark {
		v = 1
	}
	application.InvokeSync(func() { dwmSet(hwndOf(w), dwmwaUseImmersiveDark, v) })
}

var (
	procSetFocus = user32.NewProc("SetFocus")
)

const (
	wsExToolWindow = 0x00000080
	wsExAppWindow  = 0x00040000
)

func nativeBlur(w *application.WebviewWindow) {
	application.InvokeSync(func() { procSetFocus.Call(0) })
}

// nativeSetSkipTaskbar swaps the app and tool window styles so the taskbar drops or shows the button
func nativeSetSkipTaskbar(w *application.WebviewWindow, skip bool) {
	application.InvokeSync(func() {
		h := hwndOf(w)
		ex, _, _ := procGetWindowLongPtrW.Call(h, gwlExStyle)
		if skip {
			ex = ex&^wsExAppWindow | wsExToolWindow
		} else {
			ex = ex&^wsExToolWindow | wsExAppWindow
		}
		procSetWindowLongPtrW.Call(h, gwlExStyle, ex)
	})
}

func nativeDockShow() { panic("the dock exists only on macOS") }

func nativeDockHide() { panic("the dock exists only on macOS") }

func nativeSendTextAction(string) { panic("text substitution actions exist only on macOS") }
