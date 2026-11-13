//go:build linux

package main

/*
#cgo pkg-config: gtk+-3.0 webkit2gtk-4.1
#include <gtk/gtk.h>
#include <webkit2/webkit2.h>

extern void icebathRendererGone(guintptr id);

static GtkWidget *icebath_sys_find_webview(GtkWidget *w) {
	if (WEBKIT_IS_WEB_VIEW(w)) {
		return w;
	}
	if (!GTK_IS_CONTAINER(w)) {
		return NULL;
	}
	GList *children = gtk_container_get_children(GTK_CONTAINER(w));
	GtkWidget *found = NULL;
	for (GList *l = children; l != NULL && found == NULL; l = l->next) {
		found = icebath_sys_find_webview(GTK_WIDGET(l->data));
	}
	g_list_free(children);
	return found;
}

static const char *icebath_throttle_features[] = {
	"HiddenPageDOMTimerThrottling",
	"HiddenPageDOMTimerThrottlingAutoIncreases",
	"HiddenPageCSSAnimationSuspension",
	"PageVisibilityBasedProcessSuppression",
	NULL,
};

// Restores the WebKit defaults of the hidden page throttling features when allowed, turns them off otherwise
static void icebath_set_background_throttling(void *window, gboolean allowed) {
	GtkWidget *webview = icebath_sys_find_webview(GTK_WIDGET(window));
	if (webview == NULL) {
		return;
	}
	WebKitSettings *settings = webkit_web_view_get_settings(WEBKIT_WEB_VIEW(webview));
	WebKitFeatureList *list = webkit_settings_get_all_features();
	gsize n = webkit_feature_list_get_length(list);
	for (gsize i = 0; i < n; i++) {
		WebKitFeature *feature = webkit_feature_list_get(list, i);
		const char *id = webkit_feature_get_identifier(feature);
		for (int k = 0; icebath_throttle_features[k] != NULL; k++) {
			if (g_strcmp0(id, icebath_throttle_features[k]) == 0) {
				webkit_settings_set_feature_enabled(settings, feature, allowed ? webkit_feature_get_default_value(feature) : FALSE);
			}
		}
	}
	webkit_feature_list_unref(list);
}

static void icebath_web_process_terminated(WebKitWebView *view, WebKitWebProcessTerminationReason reason, gpointer id) {
	icebathRendererGone((guintptr)id);
}

static void icebath_watch_renderer(void *window, guintptr id) {
	GtkWidget *webview = icebath_sys_find_webview(GTK_WIDGET(window));
	if (webview != NULL) {
		g_signal_connect(webview, "web-process-terminated", G_CALLBACK(icebath_web_process_terminated), (gpointer)id);
	}
}

// PNG bytes of the clipboard, its image/png target first and any image GTK can read encoded second
static guchar *icebath_clipboard_png(gsize *len) {
	GtkClipboard *clipboard = gtk_clipboard_get(GDK_SELECTION_CLIPBOARD);
	GtkSelectionData *data = gtk_clipboard_wait_for_contents(clipboard, gdk_atom_intern_static_string("image/png"));
	if (data != NULL) {
		gint n = gtk_selection_data_get_length(data);
		if (n > 0) {
			guchar *out = g_memdup2(gtk_selection_data_get_data(data), n);
			*len = n;
			gtk_selection_data_free(data);
			return out;
		}
		gtk_selection_data_free(data);
	}
	GdkPixbuf *pixbuf = gtk_clipboard_wait_for_image(clipboard);
	if (pixbuf == NULL) {
		return NULL;
	}
	gchar *buffer = NULL;
	gsize size = 0;
	gboolean ok = gdk_pixbuf_save_to_buffer(pixbuf, &buffer, &size, "png", NULL, NULL);
	g_object_unref(pixbuf);
	if (!ok) {
		return NULL;
	}
	*len = size;
	return (guchar *)buffer;
}

static void icebath_set_urgency(void *window, gboolean on) {
	gtk_window_set_urgency_hint(GTK_WINDOW(window), on);
}

static gboolean icebath_display_is_wayland(void) {
	GdkDisplay *display = gdk_display_get_default();
	return display != NULL && g_strcmp0(G_OBJECT_TYPE_NAME(display), "GdkWaylandDisplay") == 0;
}
*/
import "C"

import (
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/desktop/system"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// Reads the clipboard image through GTK on the UI thread
func nativeClipboardPNG() []byte {
	var n C.gsize
	p := C.icebath_clipboard_png(&n)
	if p == nil {
		return nil
	}
	defer C.g_free(C.gpointer(p))
	return C.GoBytes(unsafe.Pointer(p), C.int(n))
}

// Sets the urgency hint, the X11 and Wayland attention request Electron flashFrame raises
func nativeFlashFrame(sw *shellWindow, on bool) {
	application.InvokeAsync(func() {
		if p := sw.w.NativeWindow(); p != nil && !sw.IsDestroyed() {
			C.icebath_set_urgency(p, C.gboolean(boolInt(on)))
		}
	})
}

// Notifications go to the session notification daemon over D-Bus
func nativeNotifier() system.Notifier { return system.NewFreedesktopNotifier() }

// Command screenshots exist only on macOS
func nativeScreenshots() system.ScreenshotNative { return nil }

// Native gesture helpers ship only in the macOS bundle
func nativeHelperPath(string) string { return "" }

// The D-Bus notifier needs no Wails service
func (s *Shell) registerSystemNatives() {}

func nativeSetBackgroundThrottling(sw *shellWindow, allowed bool) {
	if p := sw.w.NativeWindow(); p != nil {
		C.icebath_set_background_throttling(p, C.gboolean(boolInt(allowed)))
	}
}

// The webview keeps its throttling settings when its window hides, nothing to reassert
func nativeInstallThrottle(*throttleWindow) {}

// GDK opened a Wayland display, the native Wayland fullscreen case Electron keeps painting
func nativeKeepFullscreenPainting() bool { return C.icebath_display_is_wayland() != 0 }

var rendererGone struct {
	sync.Mutex
	byID map[uintptr]func()
}

//export icebathRendererGone
func icebathRendererGone(id C.guintptr) {
	rendererGone.Lock()
	f := rendererGone.byID[uintptr(id)]
	rendererGone.Unlock()
	if f != nil {
		go f()
	}
}

// Calls f each time the web process of the window terminates
func nativeOnRendererGone(sw *shellWindow, f func()) {
	id := uintptr(sw.w.ID())
	rendererGone.Lock()
	if rendererGone.byID == nil {
		rendererGone.byID = map[uintptr]func(){}
	}
	rendererGone.byID[id] = f
	rendererGone.Unlock()
	sw.onDestroyed(func() {
		rendererGone.Lock()
		delete(rendererGone.byID, id)
		rendererGone.Unlock()
	})
	var watch sync.Once
	sw.onRuntimeReady(func() {
		watch.Do(func() {
			application.InvokeAsync(func() {
				if p := sw.w.NativeWindow(); p != nil {
					C.icebath_watch_renderer(p, C.guintptr(id))
				}
			})
		})
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
