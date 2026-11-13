//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <objc/message.h>
#include <stdlib.h>
#include <stdbool.h>
#include <stdint.h>

extern bool icebathChromeMacKey(uintptr_t window, bool down, char *chars, unsigned short keyCode, bool control, bool shift, bool alt, bool meta, bool repeat);

static void icebath_chrome_monitor_keys(void) {
	[NSEvent addLocalMonitorForEventsMatchingMask:(NSEventMaskKeyDown | NSEventMaskKeyUp) handler:^NSEvent *(NSEvent *event) {
		NSString *chars = [event charactersIgnoringModifiers] ?: @"";
		bool down = [event type] == NSEventTypeKeyDown;
		NSEventModifierFlags flags = [event modifierFlags];
		if (icebathChromeMacKey((uintptr_t)[event window], down, (char *)[chars UTF8String], [event keyCode],
				(flags & NSEventModifierFlagControl) != 0, (flags & NSEventModifierFlagShift) != 0,
				(flags & NSEventModifierFlagOption) != 0, (flags & NSEventModifierFlagCommand) != 0, down && [event isARepeat])) {
			return nil;
		}
		return event;
	}];
}

static bool icebath_chrome_inspector(void *window, int op) {
	NSWindow *win = (NSWindow *)window;
	if (![win respondsToSelector:@selector(webView)]) {
		return false;
	}
	WKWebView *view = ((WKWebView *(*)(id, SEL))objc_msgSend)(win, @selector(webView));
	if (view == nil) {
		return false;
	}
	[view.configuration.preferences setValue:@YES forKey:@"developerExtrasEnabled"];
	id inspector = ((id (*)(id, SEL))objc_msgSend)(view, sel_registerName("_inspector"));
	if (inspector == nil) {
		return false;
	}
	if (op == 1) {
		((void (*)(id, SEL))objc_msgSend)(inspector, sel_registerName("show"));
		((void (*)(id, SEL))objc_msgSend)(inspector, sel_registerName("detach"));
		return true;
	}
	if (op == 2) {
		((void (*)(id, SEL))objc_msgSend)(inspector, sel_registerName("close"));
		return true;
	}
	return ((BOOL (*)(id, SEL))objc_msgSend)(inspector, sel_registerName("isVisible"));
}

static void icebath_chrome_edit(void *window, const char *selector) {
	NSWindow *win = (NSWindow *)window;
	if (![win respondsToSelector:@selector(webView)]) {
		return;
	}
	WKWebView *view = ((WKWebView *(*)(id, SEL))objc_msgSend)(win, @selector(webView));
	SEL action = sel_registerName(selector);
	if (view != nil && [view respondsToSelector:action]) {
		((void (*)(id, SEL, id))objc_msgSend)(view, action, nil);
	}
}
*/
import "C"

import (
	"strings"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/desktop/chrome"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// WKWebView responder actions of the editing commands
var wkEditingSelectors = map[string]string{"Copy": "copy:", "Cut": "cut:", "Paste": "paste:", "SelectAll": "selectAll:"}

// Sends the WKWebView responder action of an editing command to the window webview
func nativeEditingCommand(w *application.WebviewWindow, command string) {
	selector, ok := wkEditingSelectors[command]
	if !ok {
		return
	}
	application.InvokeSync(func() {
		if p := w.NativeWindow(); p != nil {
			c := C.CString(selector)
			defer C.free(unsafe.Pointer(c))
			C.icebath_chrome_edit(p, c)
		}
	})
}

// Key hooks of the chat windows by NSWindow
var macChromeHooks struct {
	sync.Mutex
	byWindow map[uintptr]func(chrome.KeyInput) bool
	monitor  sync.Once
}

//export icebathChromeMacKey
func icebathChromeMacKey(window C.uintptr_t, down C.bool, chars *C.char, keyCode C.ushort, control, shift, alt, meta, repeat C.bool) C.bool {
	macChromeHooks.Lock()
	hook := macChromeHooks.byWindow[uintptr(window)]
	macChromeHooks.Unlock()
	if hook == nil {
		return false
	}
	key := C.GoString(chars)
	code := key
	switch {
	case keyCode == 0x6F:
		key, code = "F12", "F12"
	case len(key) == 1 && key[0] >= 'a' && key[0] <= 'z':
		code = "Key" + strings.ToUpper(key)
	}
	input := chrome.KeyInput{Type: "keyUp", Key: key, Code: code, IsAutoRepeat: bool(repeat),
		Control: bool(control), Shift: bool(shift), Alt: bool(alt), Meta: bool(meta)}
	if down {
		input.Type = "keyDown"
		if input.Shift && len(key) == 1 {
			input.Key = strings.ToUpper(key)
		}
	}
	return C.bool(hook(input))
}

// Routes the key events of the window through hook ahead of its WKWebView
func nativeInstallKeyHook(sw *shellWindow, hook func(chrome.KeyInput) bool) {
	macChromeHooks.monitor.Do(func() { application.InvokeSync(func() { C.icebath_chrome_monitor_keys() }) })
	var window uintptr
	application.InvokeSync(func() { window = uintptr(sw.w.NativeWindow()) })
	if window == 0 {
		return
	}
	macChromeHooks.Lock()
	if macChromeHooks.byWindow == nil {
		macChromeHooks.byWindow = map[uintptr]func(chrome.KeyInput) bool{}
	}
	macChromeHooks.byWindow[window] = hook
	macChromeHooks.Unlock()
	sw.onDestroyed(func() {
		macChromeHooks.Lock()
		delete(macChromeHooks.byWindow, window)
		macChromeHooks.Unlock()
	})
}

func inspector(sw *shellWindow, op C.int) bool {
	var result bool
	application.InvokeSync(func() {
		if p := sw.w.NativeWindow(); p != nil {
			result = bool(C.icebath_chrome_inspector(p, op))
		}
	})
	return result
}

// Whether the Web Inspector of the window is visible
func nativeDevToolsOpened(sw *shellWindow) bool { return inspector(sw, 0) }

// Shows the Web Inspector of the window detached
func nativeOpenDevTools(sw *shellWindow) { inspector(sw, 1) }

// Closes the Web Inspector of the window
func nativeCloseDevTools(sw *shellWindow) { inspector(sw, 2) }
