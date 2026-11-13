//go:build windows

package main

import (
	"reflect"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/desktop/system"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// WebView2 controller of a Wails window, invalid while the controller is being created or rebuilt
func webview2Chromium(w *application.WebviewWindow) reflect.Value {
	impl := reflect.ValueOf(w).Elem().FieldByName("impl")
	if !impl.IsValid() || impl.IsNil() {
		return reflect.Value{}
	}
	native := impl.Elem()
	if native.Kind() != reflect.Pointer || native.IsNil() {
		return reflect.Value{}
	}
	field := native.Elem().FieldByName("chromium")
	if !field.IsValid() || field.IsNil() {
		return reflect.Value{}
	}
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
}

func chromiumCall(sw *shellWindow, method string) {
	if c := webview2Chromium(sw.w); c.IsValid() {
		c.MethodByName(method).Call(nil)
	}
}

// Keeps the WebView2 controller visible while unthrottled, hiding it again for a hidden window once allowed
func nativeSetBackgroundThrottling(sw *shellWindow, allowed bool) {
	if !allowed {
		chromiumCall(sw, "Show")
		return
	}
	if !sw.w.IsVisible() {
		chromiumCall(sw, "Hide")
	}
}

// Reasserts the visible controller after Wails hides it with its window while unthrottled
func nativeInstallThrottle(tw *throttleWindow) {
	tw.sw.w.OnWindowEvent(events.Common.WindowHide, func(*application.WindowEvent) {
		if !tw.allowed.Load() {
			tw.SetBackgroundThrottling(false)
		}
	})
}

// Fullscreen painting is a native Wayland concern only
func nativeKeepFullscreenPainting() bool { return false }

// COREWEBVIEW2_PROCESS_FAILED_KIND values whose exit takes the page renderer with it
const (
	processFailedBrowserExited = 0
	processFailedRenderExited  = 1
)

// Calls f when the browser or render process of the window exits, rewrapping a rebuilt controller on load
func nativeOnRendererGone(sw *shellWindow, f func()) {
	var mu sync.Mutex
	var wrapped uintptr
	sw.onRuntimeReady(func() {
		application.InvokeAsync(func() {
			c := webview2Chromium(sw.w)
			if !c.IsValid() {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if c.Pointer() == wrapped {
				return
			}
			field := c.Elem().FieldByName("ProcessFailedCallback")
			original := field.Interface()
			field.Set(reflect.MakeFunc(field.Type(), func(args []reflect.Value) []reflect.Value {
				if out := args[1].MethodByName("GetProcessFailedKind").Call(nil); out[1].IsNil() {
					if kind := out[0].Uint(); kind == processFailedBrowserExited || kind == processFailedRenderExited {
						go f()
					}
				}
				if original != nil && !reflect.ValueOf(original).IsNil() {
					return reflect.ValueOf(original).Call(args)
				}
				return nil
			}))
			wrapped = c.Pointer()
		})
	})
}

// Reads the clipboard image, the registered PNG format or the device independent bitmap
func nativeClipboardPNG() []byte { return system.PlatformClipboardPNG() }

// Flashes the taskbar button, the Windows flashFrame
func nativeFlashFrame(sw *shellWindow, on bool) { sw.w.Flash(on) }

// Command screenshots exist only on macOS
func nativeScreenshots() system.ScreenshotNative { return nil }

// Native gesture helpers ship only in the macOS bundle
func nativeHelperPath(string) string { return "" }
