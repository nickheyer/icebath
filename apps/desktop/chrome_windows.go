//go:build windows

package main

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/desktop/chrome"
	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/sys/windows"
)

// Clipboard and selection scripts WebView2 runs for the edit verbs, it exposes no native editing command
var webview2EditingScripts = map[string]string{
	"Cut":       "document.execCommand('cut')",
	"SelectAll": "document.execCommand('selectAll')",
	"Copy": `(async () => {
	try {
		const selection = window.getSelection();
		if (!selection.rangeCount) return;
		const container = document.createElement('div');
		container.appendChild(selection.getRangeAt(0).cloneContents());
		const images = container.getElementsByTagName('img');
		if (images.length > 0) {
			const blob = await (await fetch(images[0].src)).blob();
			await navigator.clipboard.write([new ClipboardItem({ [blob.type]: blob })]);
		} else {
			const text = selection.toString();
			if (text) await navigator.clipboard.writeText(text);
		}
	} catch (err) {
		console.error('Failed to copy:', err);
	}
})()`,
	"Paste": `(async () => {
	try {
		for (const item of await navigator.clipboard.read()) {
			for (const type of item.types) {
				if (type.startsWith('image/')) {
					const url = URL.createObjectURL(await item.getType(type));
					document.execCommand('insertHTML', false, '<img src="' + url + '">');
					return;
				}
			}
			if (item.types.includes('text/plain')) {
				document.execCommand('insertText', false, await navigator.clipboard.readText());
				return;
			}
		}
	} catch (err) {
		try {
			document.execCommand('insertText', false, await navigator.clipboard.readText());
		} catch (fallbackErr) {
			console.error('Failed to paste:', err, fallbackErr);
		}
	}
})()`,
}

// Runs the WebView2 script of an editing command against the focused element
func nativeEditingCommand(w *application.WebviewWindow, command string) {
	if script, ok := webview2EditingScripts[command]; ok {
		w.ExecJS(script)
	}
}

var (
	chromeUser32                   = windows.NewLazySystemDLL("user32.dll")
	chromeGetKeyState              = chromeUser32.NewProc("GetKeyState")
	chromeEnumWindows              = chromeUser32.NewProc("EnumWindows")
	chromeGetWindowTextW           = chromeUser32.NewProc("GetWindowTextW")
	chromeGetWindowThreadProcessID = chromeUser32.NewProc("GetWindowThreadProcessId")
	chromeIsWindow                 = chromeUser32.NewProc("IsWindow")
	chromePostMessageW             = chromeUser32.NewProc("PostMessageW")
)

// Vtable slot of ICoreWebView2 get_BrowserProcessId
const browserProcessIDSlot = 37

// Chromium controllers already hooked and the DevTools window each chat window opened
var webview2Chrome = struct {
	sync.Mutex
	hooked   map[uintptr]bool
	devtools map[uintptr]windows.HWND
}{hooked: map[uintptr]bool{}, devtools: map[uintptr]windows.HWND{}}

func keyHeld(vk uintptr) bool {
	r, _, _ := chromeGetKeyState.Call(vk)
	return r&0x8000 != 0
}

// Electron input of a WebView2 accelerator keydown with the modifier state of the UI thread
func webview2KeyInput(vk uint) chrome.KeyInput {
	in := chrome.KeyInput{Type: "keyDown", Control: keyHeld(0x11), Shift: keyHeld(0x10), Alt: keyHeld(0x12), Meta: keyHeld(0x5B) || keyHeld(0x5C)}
	switch {
	case vk >= 0x70 && vk <= 0x87:
		in.Key = fmt.Sprintf("F%d", vk-0x6F)
		in.Code = in.Key
	case vk >= 0x41 && vk <= 0x5A:
		upper := string(rune(vk))
		in.Key, in.Code = strings.ToLower(upper), "Key"+upper
		if in.Shift {
			in.Key = upper
		}
	case vk >= 0x30 && vk <= 0x39:
		in.Key, in.Code = string(rune(vk)), "Digit"+string(rune(vk))
	}
	return in
}

// Wraps the WebView2 accelerator callback of the window so hook sees each keydown before Wails and the page
func nativeInstallKeyHook(sw *shellWindow, hook func(chrome.KeyInput) bool) {
	id := uintptr(sw.w.ID())
	sw.onDestroyed(func() {
		webview2Chrome.Lock()
		delete(webview2Chrome.devtools, id)
		webview2Chrome.Unlock()
	})
	sw.onRuntimeReady(func() {
		application.InvokeAsync(func() {
			c := webview2Chromium(sw.w)
			if !c.IsValid() || c.IsNil() {
				return
			}
			webview2Chrome.Lock()
			ptr := c.Pointer()
			already := webview2Chrome.hooked[ptr]
			webview2Chrome.hooked[ptr] = true
			webview2Chrome.Unlock()
			if already {
				return
			}
			field := c.Elem().FieldByName("AcceleratorKeyCallback")
			next, _ := field.Interface().(func(uint) bool)
			field.Set(reflect.ValueOf(func(vk uint) bool {
				if hook(webview2KeyInput(vk)) {
					return true
				}
				return next != nil && next(vk)
			}))
		})
	})
}

// Process id of the WebView2 browser process behind the window, zero while it has no webview
func webview2BrowserPID(sw *shellWindow) uint32 {
	c := webview2Chromium(sw.w)
	if !c.IsValid() || c.IsNil() {
		return 0
	}
	field := c.Elem().FieldByName("webview")
	webview := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	if webview.IsNil() {
		return 0
	}
	object := webview.UnsafePointer()
	vtbl := *(*[browserProcessIDSlot + 1]uintptr)(*(*unsafe.Pointer)(object))
	var pid uint32
	syscall.SyscallN(vtbl[browserProcessIDSlot], uintptr(object), uintptr(unsafe.Pointer(&pid)))
	return pid
}

// Top level DevTools windows the browser process owns
func devToolsWindows(pid uint32) map[windows.HWND]bool {
	found := map[windows.HWND]bool{}
	cb := syscall.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		var owner uint32
		chromeGetWindowThreadProcessID.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&owner)))
		if owner == pid {
			buf := make([]uint16, 256)
			n, _, _ := chromeGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
			if strings.HasPrefix(windows.UTF16ToString(buf[:n]), "DevTools") {
				found[hwnd] = true
			}
		}
		return 1
	})
	chromeEnumWindows.Call(cb, 0)
	return found
}

// Whether the DevTools window this chat window opened is still open
func nativeDevToolsOpened(sw *shellWindow) bool {
	var open bool
	application.InvokeSync(func() {
		webview2Chrome.Lock()
		hwnd := webview2Chrome.devtools[uintptr(sw.w.ID())]
		webview2Chrome.Unlock()
		if hwnd != 0 {
			r, _, _ := chromeIsWindow.Call(uintptr(hwnd))
			open = r != 0
		}
	})
	return open
}

// Opens the WebView2 DevTools window and claims the new DevTools window of the browser process for this chat window
func nativeOpenDevTools(sw *shellWindow) {
	var pid uint32
	var before map[windows.HWND]bool
	application.InvokeSync(func() {
		pid = webview2BrowserPID(sw)
		before = devToolsWindows(pid)
		if c := webview2Chromium(sw.w); c.IsValid() && !c.IsNil() {
			c.MethodByName("OpenDevToolsWindow").Call(nil)
		}
	})
	if pid == 0 {
		return
	}
	go func() {
		for !sw.IsDestroyed() {
			time.Sleep(100 * time.Millisecond)
			var claimed windows.HWND
			application.InvokeSync(func() {
				for hwnd := range devToolsWindows(pid) {
					if !before[hwnd] {
						claimed = hwnd
					}
				}
			})
			if claimed != 0 {
				webview2Chrome.Lock()
				webview2Chrome.devtools[uintptr(sw.w.ID())] = claimed
				webview2Chrome.Unlock()
				return
			}
		}
	}()
}

// Closes the DevTools window this chat window opened
func nativeCloseDevTools(sw *shellWindow) {
	webview2Chrome.Lock()
	hwnd := webview2Chrome.devtools[uintptr(sw.w.ID())]
	delete(webview2Chrome.devtools, uintptr(sw.w.ID()))
	webview2Chrome.Unlock()
	if hwnd != 0 {
		chromePostMessageW.Call(uintptr(hwnd), 0x0010, 0, 0)
	}
}
