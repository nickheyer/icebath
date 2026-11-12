//go:build linux

package main

/*
#cgo pkg-config: gtk+-3.0
#include <gtk/gtk.h>
#include <string.h>
#include <stdlib.h>

static void icebath_show_inactive(void *w) {
	GtkWindow *win = GTK_WINDOW(w);
	gtk_window_set_focus_on_map(win, FALSE);
	gtk_widget_show(GTK_WIDGET(win));
	gtk_window_set_focus_on_map(win, TRUE);
}

static void icebath_set_opacity(void *w, double o) {
	gtk_widget_set_opacity(GTK_WIDGET(w), o);
}

static char *icebath_theme_name(void) {
	gchar *name = NULL;
	g_object_get(gtk_settings_get_default(), "gtk-theme-name", &name, NULL);
	return name;
}

static gboolean icebath_prefer_dark(void) {
	gboolean dark = FALSE;
	g_object_get(gtk_settings_get_default(), "gtk-application-prefer-dark-theme", &dark, NULL);
	return dark;
}

static void icebath_set_theme(const char *name, gboolean dark) {
	GtkSettings *s = gtk_settings_get_default();
	if (name != NULL) {
		g_object_set(s, "gtk-theme-name", name, NULL);
	}
	g_object_set(s, "gtk-application-prefer-dark-theme", dark, NULL);
}
*/
import "C"

import (
	"strings"
	"sync"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

var linuxTheme struct {
	sync.Mutex
	mode     string
	started  bool
	captured bool
	origName string
	origDark bool
}

func nativeShowInactive(w *application.WebviewWindow) {
	application.InvokeSync(func() {
		if p := w.NativeWindow(); p != nil {
			C.icebath_show_inactive(p)
		}
	})
}

func nativeSetOpacity(w *application.WebviewWindow, o float64) {
	application.InvokeSync(func() {
		if p := w.NativeWindow(); p != nil {
			C.icebath_set_opacity(p, C.double(o))
		}
	})
}

func nativeSetVibrancy(*application.WebviewWindow, string) {
	panic("vibrancy is a macOS window material and is never applied on Linux")
}

func nativeSetBackgroundMaterial(*application.WebviewWindow, string) {
	panic("backdrop materials are a Windows window feature and are never applied on Linux")
}

func nativeSetTrafficLightInset(*application.WebviewWindow, float64) {
	panic("traffic light insets exist only on macOS")
}

// nativeApplyWindowTheme has nothing per window to do, the GTK settings object themes every window
func nativeApplyWindowTheme(*application.WebviewWindow, bool) {}

func nativeThemeSource() string {
	linuxTheme.Lock()
	defer linuxTheme.Unlock()
	if linuxTheme.mode == "" {
		return "system"
	}
	return linuxTheme.mode
}

// nativeSetThemeSource records the mode and applies it to GTK once the toolkit runs
func nativeSetThemeSource(mode string) {
	linuxTheme.Lock()
	linuxTheme.mode = mode
	started := linuxTheme.started
	linuxTheme.Unlock()
	if started {
		application.InvokeSync(applyLinuxTheme)
	}
}

// nativeThemeStarted applies the recorded theme source after GTK is initialized
func nativeThemeStarted() {
	linuxTheme.Lock()
	linuxTheme.started = true
	linuxTheme.Unlock()
	application.InvokeSync(applyLinuxTheme)
}

func applyLinuxTheme() {
	linuxTheme.Lock()
	defer linuxTheme.Unlock()
	if !linuxTheme.captured {
		name := C.icebath_theme_name()
		if name != nil {
			linuxTheme.origName = C.GoString(name)
			C.g_free(C.gpointer(unsafe.Pointer(name)))
		}
		linuxTheme.origDark = C.icebath_prefer_dark() != 0
		linuxTheme.captured = true
	}
	setTheme := func(name string, dark bool) {
		var cname *C.char
		if name != "" {
			cname = C.CString(name)
			defer C.free(unsafe.Pointer(cname))
		}
		d := C.gboolean(0)
		if dark {
			d = 1
		}
		C.icebath_set_theme(cname, d)
	}
	switch linuxTheme.mode {
	case "dark":
		setTheme(linuxTheme.origName, true)
	case "light":
		setTheme(strings.TrimSuffix(strings.TrimSuffix(linuxTheme.origName, "-dark"), "-Dark"), false)
	default:
		setTheme(linuxTheme.origName, linuxTheme.origDark)
	}
}

func nativeBlur(*application.WebviewWindow) { panic("tray blur runs only on Windows") }

func nativeSetSkipTaskbar(*application.WebviewWindow, bool) {
	panic("tray taskbar skipping runs only on Windows")
}

func nativeDockShow() { panic("the dock exists only on macOS") }

func nativeDockHide() { panic("the dock exists only on macOS") }

func nativeSendTextAction(string) { panic("text substitution actions exist only on macOS") }
