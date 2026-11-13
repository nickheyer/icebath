//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <objc/message.h>

static WKWebView *icebath_sys_webview(void *window) {
	NSWindow *win = (NSWindow *)window;
	if (![win respondsToSelector:@selector(webView)]) {
		return nil;
	}
	return ((WKWebView *(*)(id, SEL))objc_msgSend)(win, @selector(webView));
}

// Restores the WebKit defaults of the inactive scheduling and hidden page throttling preferences when allowed
static void icebath_set_background_throttling(void *window, bool allowed) {
	WKWebView *view = icebath_sys_webview(window);
	if (view == nil) {
		return;
	}
	WKPreferences *prefs = view.configuration.preferences;
	WKPreferences *defaults = [[WKPreferences alloc] init];
	if (@available(macOS 14.0, *)) {
		prefs.inactiveSchedulingPolicy = allowed ? defaults.inactiveSchedulingPolicy : WKInactiveSchedulingPolicyNone;
	}
	NSArray<NSString *> *keys = @[@"HiddenPageDOMTimerThrottlingEnabled", @"HiddenPageDOMTimerThrottlingAutoIncreases", @"HiddenPageCSSAnimationSuspensionEnabled", @"PageVisibilityBasedProcessSuppressionEnabled"];
	for (NSString *key in keys) {
		NSString *lower = [[[key substringToIndex:1] lowercaseString] stringByAppendingString:[key substringFromIndex:1]];
		SEL getter = NSSelectorFromString([@"_" stringByAppendingString:lower]);
		SEL setter = NSSelectorFromString([NSString stringWithFormat:@"_set%@:", key]);
		if (![prefs respondsToSelector:setter] || ![defaults respondsToSelector:getter]) {
			continue;
		}
		BOOL value = allowed ? ((BOOL (*)(id, SEL))objc_msgSend)(defaults, getter) : NO;
		((void (*)(id, SEL, BOOL))objc_msgSend)(prefs, setter, value);
	}
	[defaults release];
}
*/
import "C"

import (
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/desktop/system"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// Reads the general pasteboard image
func nativeClipboardPNG() []byte { return system.PlatformClipboardPNG() }

// Requests or cancels dock attention, the macOS flashFrame
func nativeFlashFrame(sw *shellWindow, on bool) { sw.w.Flash(on) }

// Window capture through ScreenCaptureKit and CoreGraphics
func nativeScreenshots() system.ScreenshotNative { return system.NewScreenshotNative() }

// Native helper in the bundle Resources/native, or the dev build under dist/native beside the working directory
func nativeHelperPath(name string) string {
	if exe, err := os.Executable(); err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
		candidate := filepath.Join(filepath.Dir(exe), "..", "Resources", "native", name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	wd, _ := os.Getwd()
	return filepath.Join(wd, "dist", "native", name)
}

func nativeSetBackgroundThrottling(sw *shellWindow, allowed bool) {
	if p := sw.w.NativeWindow(); p != nil {
		C.icebath_set_background_throttling(p, C.bool(allowed))
	}
}

// WKWebView keeps its preferences when its window hides, nothing to reassert
func nativeInstallThrottle(*throttleWindow) {}

// Fullscreen painting is a native Wayland concern only
func nativeKeepFullscreenPainting() bool { return false }

// Calls f each time the WKWebView content process terminates
func nativeOnRendererGone(sw *shellWindow, f func()) {
	sw.w.OnWindowEvent(events.Mac.WebViewWebContentProcessDidTerminate, func(*application.WindowEvent) { f() })
}
