//go:build linux

package main

/*
#cgo pkg-config: gtk+-3.0 webkit2gtk-4.1
#include <gtk/gtk.h>
#include <webkit2/webkit2.h>
#include <stdlib.h>

static GtkWidget *icebath_chrome_find_webview(GtkWidget *w) {
	if (WEBKIT_IS_WEB_VIEW(w)) {
		return w;
	}
	if (!GTK_IS_CONTAINER(w)) {
		return NULL;
	}
	GList *children = gtk_container_get_children(GTK_CONTAINER(w));
	GtkWidget *found = NULL;
	for (GList *l = children; l != NULL && found == NULL; l = l->next) {
		found = icebath_chrome_find_webview(GTK_WIDGET(l->data));
	}
	g_list_free(children);
	return found;
}

extern gboolean icebathChromeKey(guintptr id, gboolean press, guint keyval, guint16 hardware, guint state);
extern void icebathChromeInspectorClosed(guintptr id);

static gboolean icebath_chrome_key(GtkWidget *webview, GdkEventKey *event, gpointer data) {
	return icebathChromeKey((guintptr)data, event->type == GDK_KEY_PRESS, event->keyval, event->hardware_keycode, event->state);
}

static void icebath_chrome_inspector_closed(WebKitWebInspector *inspector, gpointer data) {
	icebathChromeInspectorClosed((guintptr)data);
}

static void icebath_chrome_install(void *window, guintptr id) {
	GtkWidget *webview = icebath_chrome_find_webview(GTK_WIDGET(window));
	if (webview == NULL) {
		return;
	}
	g_signal_connect(webview, "key-press-event", G_CALLBACK(icebath_chrome_key), (gpointer)id);
	g_signal_connect(webview, "key-release-event", G_CALLBACK(icebath_chrome_key), (gpointer)id);
	webkit_settings_set_enable_developer_extras(webkit_web_view_get_settings(WEBKIT_WEB_VIEW(webview)), TRUE);
	g_signal_connect(webkit_web_view_get_inspector(WEBKIT_WEB_VIEW(webview)), "closed", G_CALLBACK(icebath_chrome_inspector_closed), (gpointer)id);
}

static void icebath_chrome_inspector(void *window, gboolean open) {
	GtkWidget *webview = icebath_chrome_find_webview(GTK_WIDGET(window));
	if (webview == NULL) {
		return;
	}
	WebKitWebInspector *inspector = webkit_web_view_get_inspector(WEBKIT_WEB_VIEW(webview));
	if (open) {
		webkit_web_inspector_show(inspector);
		webkit_web_inspector_detach(inspector);
	} else {
		webkit_web_inspector_close(inspector);
	}
}

static void icebath_chrome_edit(void *window, const char *command) {
	GtkWidget *webview = icebath_chrome_find_webview(GTK_WIDGET(window));
	if (webview != NULL) {
		webkit_web_view_execute_editing_command(WEBKIT_WEB_VIEW(webview), command);
	}
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unicode"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/desktop/chrome"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// Key hooks, held keys and open inspectors of the chat windows by Wails window id
var chromeWindows struct {
	sync.Mutex
	hooks     map[uintptr]func(chrome.KeyInput) bool
	held      map[uintptr]map[uint16]bool
	inspector map[uintptr]bool
}

// DOM key and code of a GDK keyval, function keys by number, letters and digits by character
func gdkKeyNames(keyval C.guint) (string, string) {
	if keyval >= C.GDK_KEY_F1 && keyval <= C.GDK_KEY_F35 {
		name := fmt.Sprintf("F%d", int(keyval-C.GDK_KEY_F1)+1)
		return name, name
	}
	r := rune(C.gdk_keyval_to_unicode(keyval))
	if r == 0 {
		return C.GoString(C.gdk_keyval_name(keyval)), C.GoString(C.gdk_keyval_name(keyval))
	}
	code := string(r)
	switch {
	case unicode.IsLetter(r) && r < unicode.MaxASCII:
		code = "Key" + string(unicode.ToUpper(r))
	case r >= '0' && r <= '9':
		code = "Digit" + string(r)
	}
	return string(r), code
}

//export icebathChromeKey
func icebathChromeKey(id C.guintptr, press C.gboolean, keyval C.guint, hardware C.guint16, state C.guint) C.gboolean {
	chromeWindows.Lock()
	hook := chromeWindows.hooks[uintptr(id)]
	held := chromeWindows.held[uintptr(id)]
	repeat := false
	if held != nil {
		repeat = press != 0 && held[uint16(hardware)]
		if press != 0 {
			held[uint16(hardware)] = true
		} else {
			delete(held, uint16(hardware))
		}
	}
	chromeWindows.Unlock()
	if hook == nil {
		return C.FALSE
	}
	key, code := gdkKeyNames(keyval)
	input := chrome.KeyInput{Type: "keyUp", Key: key, Code: code, IsAutoRepeat: repeat,
		Control: state&C.GDK_CONTROL_MASK != 0, Shift: state&C.GDK_SHIFT_MASK != 0, Alt: state&C.GDK_MOD1_MASK != 0,
		Meta: state&(C.GDK_SUPER_MASK|C.GDK_META_MASK) != 0}
	if press != 0 {
		input.Type = "keyDown"
	}
	if hook(input) {
		return C.TRUE
	}
	return C.FALSE
}

//export icebathChromeInspectorClosed
func icebathChromeInspectorClosed(id C.guintptr) {
	chromeWindows.Lock()
	delete(chromeWindows.inspector, uintptr(id))
	chromeWindows.Unlock()
}

// Routes the keys of the window webview through hook before WebKit sees them, once its runtime is up
func nativeInstallKeyHook(sw *shellWindow, hook func(chrome.KeyInput) bool) {
	id := uintptr(sw.w.ID())
	chromeWindows.Lock()
	if chromeWindows.hooks == nil {
		chromeWindows.hooks, chromeWindows.held, chromeWindows.inspector = map[uintptr]func(chrome.KeyInput) bool{}, map[uintptr]map[uint16]bool{}, map[uintptr]bool{}
	}
	chromeWindows.hooks[id] = hook
	chromeWindows.held[id] = map[uint16]bool{}
	chromeWindows.Unlock()
	sw.onDestroyed(func() {
		chromeWindows.Lock()
		delete(chromeWindows.hooks, id)
		delete(chromeWindows.held, id)
		delete(chromeWindows.inspector, id)
		chromeWindows.Unlock()
	})
	var install sync.Once
	sw.onRuntimeReady(func() {
		install.Do(func() {
			application.InvokeAsync(func() {
				if p := sw.w.NativeWindow(); p != nil {
					C.icebath_chrome_install(p, C.guintptr(id))
				}
			})
		})
	})
}

// Whether the inspector of the window is open
func nativeDevToolsOpened(sw *shellWindow) bool {
	chromeWindows.Lock()
	defer chromeWindows.Unlock()
	return chromeWindows.inspector[uintptr(sw.w.ID())]
}

func setInspector(sw *shellWindow, open bool) {
	id := uintptr(sw.w.ID())
	application.InvokeSync(func() {
		if p := sw.w.NativeWindow(); p != nil {
			C.icebath_chrome_inspector(p, boolGboolean(open))
		}
	})
	chromeWindows.Lock()
	if open {
		chromeWindows.inspector[id] = true
	} else {
		delete(chromeWindows.inspector, id)
	}
	chromeWindows.Unlock()
}

// Opens the window inspector detached
func nativeOpenDevTools(sw *shellWindow) { setInspector(sw, true) }

// Closes the window inspector
func nativeCloseDevTools(sw *shellWindow) { setInspector(sw, false) }

func boolGboolean(b bool) C.gboolean {
	if b {
		return C.TRUE
	}
	return C.FALSE
}

// Runs a WebKit editing command on the focused element of the window webview
func nativeEditingCommand(w *application.WebviewWindow, command string) {
	application.InvokeSync(func() {
		if p := w.NativeWindow(); p != nil {
			c := C.CString(command)
			defer C.free(unsafe.Pointer(c))
			C.icebath_chrome_edit(p, c)
		}
	})
}
