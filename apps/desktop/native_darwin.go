//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

static void icebath_show_inactive(void *w) {
	[(NSWindow *)w orderFront:nil];
}

static void icebath_set_opacity(void *w, double o) {
	[(NSWindow *)w setAlphaValue:o];
}

static NSVisualEffectMaterial icebath_material(const char *name) {
	if (strcmp(name, "under-window") == 0) return NSVisualEffectMaterialUnderWindowBackground;
	if (strcmp(name, "popover") == 0) return NSVisualEffectMaterialPopover;
	if (strcmp(name, "titlebar") == 0) return NSVisualEffectMaterialTitlebar;
	if (strcmp(name, "header") == 0) return NSVisualEffectMaterialHeaderView;
	return NSVisualEffectMaterialSidebar;
}

static void icebath_set_vibrancy(void *w, const char *name) {
	NSWindow *win = (NSWindow *)w;
	NSView *content = [win contentView];
	NSVisualEffectView *fx = nil;
	for (NSView *v in [[content superview] subviews]) {
		if ([v isKindOfClass:[NSVisualEffectView class]] && [[v identifier] isEqualToString:@"icebath-vibrancy"]) {
			fx = (NSVisualEffectView *)v;
		}
	}
	if (fx == nil) {
		fx = [[NSVisualEffectView alloc] initWithFrame:[[content superview] bounds]];
		[fx setIdentifier:@"icebath-vibrancy"];
		[fx setAutoresizingMask:NSViewWidthSizable | NSViewHeightSizable];
		[fx setBlendingMode:NSVisualEffectBlendingModeBehindWindow];
		[fx setState:NSVisualEffectStateActive];
		[[content superview] addSubview:fx positioned:NSWindowBelow relativeTo:content];
	}
	[fx setMaterial:icebath_material(name)];
}

static void icebath_set_appearance(const char *mode) {
	if (strcmp(mode, "dark") == 0) {
		[NSApp setAppearance:[NSAppearance appearanceNamed:NSAppearanceNameDarkAqua]];
	} else if (strcmp(mode, "light") == 0) {
		[NSApp setAppearance:[NSAppearance appearanceNamed:NSAppearanceNameAqua]];
	} else {
		[NSApp setAppearance:nil];
	}
}

static void icebath_set_traffic_light_inset(void *w, double height) {
	NSWindow *win = (NSWindow *)w;
	NSButton *close = [win standardWindowButton:NSWindowCloseButton];
	if (close == nil || height <= 0) return;
	NSView *bar = [close superview];
	CGFloat y = ([bar frame].size.height - [close frame].size.height) / 2;
	for (NSButton *b in @[close, [win standardWindowButton:NSWindowMiniaturizeButton], [win standardWindowButton:NSWindowZoomButton]]) {
		NSRect f = [b frame];
		f.origin.y = y;
		[b setFrame:f];
	}
}
static void icebath_dock(int show) {
	[NSApp setActivationPolicy:(show ? NSApplicationActivationPolicyRegular : NSApplicationActivationPolicyAccessory)];
}

static void icebath_text_action(const char *name) {
	if (strcmp(name, "showSubstitutions") == 0) {
		[NSApp sendAction:@selector(orderFrontSubstitutionsPanel:) to:nil from:nil];
	} else if (strcmp(name, "toggleTextReplacement") == 0) {
		[NSApp sendAction:@selector(toggleAutomaticTextReplacement:) to:nil from:nil];
	}
}
*/
import "C"

import (
	"sync"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

var macTheme struct {
	sync.Mutex
	mode    string
	started bool
}

func nativeShowInactive(w *application.WebviewWindow) {
	application.InvokeSync(func() { C.icebath_show_inactive(w.NativeWindow()) })
}

func nativeSetOpacity(w *application.WebviewWindow, o float64) {
	application.InvokeSync(func() { C.icebath_set_opacity(w.NativeWindow(), C.double(o)) })
}

func nativeSetVibrancy(w *application.WebviewWindow, material string) {
	name := C.CString(material)
	defer C.free(unsafe.Pointer(name))
	application.InvokeSync(func() { C.icebath_set_vibrancy(w.NativeWindow(), name) })
}

func nativeSetBackgroundMaterial(*application.WebviewWindow, string) {
	panic("backdrop materials are a Windows window feature and are never applied on macOS")
}

func nativeSetTrafficLightInset(w *application.WebviewWindow, height float64) {
	application.InvokeSync(func() { C.icebath_set_traffic_light_inset(w.NativeWindow(), C.double(height)) })
}

// nativeApplyWindowTheme has nothing per window to do, NSApp appearance themes every window
func nativeApplyWindowTheme(*application.WebviewWindow, bool) {}

func nativeThemeSource() string {
	macTheme.Lock()
	defer macTheme.Unlock()
	if macTheme.mode == "" {
		return "system"
	}
	return macTheme.mode
}

func nativeSetThemeSource(mode string) {
	macTheme.Lock()
	macTheme.mode = mode
	started := macTheme.started
	macTheme.Unlock()
	if started {
		applyMacTheme(mode)
	}
}

func nativeThemeStarted() {
	macTheme.Lock()
	macTheme.started = true
	mode := macTheme.mode
	macTheme.Unlock()
	applyMacTheme(mode)
}

func applyMacTheme(mode string) {
	cmode := C.CString(mode)
	defer C.free(unsafe.Pointer(cmode))
	application.InvokeSync(func() { C.icebath_set_appearance(cmode) })
}

func nativeBlur(*application.WebviewWindow) { panic("tray blur runs only on Windows") }

func nativeSetSkipTaskbar(*application.WebviewWindow, bool) {
	panic("tray taskbar skipping runs only on Windows")
}

func nativeDockShow() { application.InvokeSync(func() { C.icebath_dock(1) }) }

func nativeDockHide() { application.InvokeSync(func() { C.icebath_dock(0) }) }

func nativeSendTextAction(name string) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	application.InvokeSync(func() { C.icebath_text_action(cname) })
}
