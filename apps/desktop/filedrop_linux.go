//go:build linux

package main

/*
#cgo pkg-config: gtk+-3.0 webkit2gtk-4.1
#include <gtk/gtk.h>
#include <webkit2/webkit2.h>
#include <stdlib.h>
#include <string.h>

extern char *icebathDroppedFilesJS(char **paths, int count);

static GtkWidget *icebath_find_webview(GtkWidget *w) {
	if (WEBKIT_IS_WEB_VIEW(w)) {
		return w;
	}
	if (!GTK_IS_CONTAINER(w)) {
		return NULL;
	}
	GList *children = gtk_container_get_children(GTK_CONTAINER(w));
	GtkWidget *found = NULL;
	for (GList *l = children; l != NULL && found == NULL; l = l->next) {
		found = icebath_find_webview(GTK_WIDGET(l->data));
	}
	g_list_free(children);
	return found;
}

static void icebath_drag_data_received(GtkWidget *widget, GdkDragContext *context, gint x, gint y,
		GtkSelectionData *data, guint info, guint time, gpointer user) {
	if (data == NULL || gtk_selection_data_get_length(data) <= 0) {
		return;
	}
	gchar *target = gdk_atom_name(gtk_selection_data_get_target(data));
	gboolean uris = target != NULL && strcmp(target, "text/uri-list") == 0;
	g_free(target);
	if (!uris) {
		return;
	}
	gchar **list = gtk_selection_data_get_uris(data);
	if (list == NULL) {
		return;
	}
	int count = 0;
	char **paths = g_new0(char *, g_strv_length(list) + 1);
	for (int i = 0; list[i] != NULL; i++) {
		char *p = g_filename_from_uri(list[i], NULL, NULL);
		if (p != NULL) {
			paths[count++] = p;
		}
	}
	g_strfreev(list);
	char *js = icebathDroppedFilesJS(paths, count);
	for (int i = 0; i < count; i++) {
		g_free(paths[i]);
	}
	g_free(paths);
	webkit_web_view_evaluate_javascript(WEBKIT_WEB_VIEW(widget), js, -1, NULL, NULL, NULL, NULL, NULL);
	free(js);
}

static void icebath_install_file_drop(void *window, guintptr id) {
	GtkWidget *webview = icebath_find_webview(GTK_WIDGET(window));
	if (webview == NULL) {
		return;
	}
	const char *blocked[] = {"drag-drop", "drag-motion"};
	for (int i = 0; i < 2; i++) {
		guint signal = g_signal_lookup(blocked[i], GTK_TYPE_WIDGET);
		g_signal_handlers_disconnect_matched(webview, G_SIGNAL_MATCH_ID | G_SIGNAL_MATCH_DATA, signal, 0, NULL, NULL, (gpointer)id);
	}
	g_signal_connect(webview, "drag-data-received", G_CALLBACK(icebath_drag_data_received), NULL);
}
*/
import "C"

import (
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//export icebathDroppedFilesJS
func icebathDroppedFilesJS(paths **C.char, count C.int) *C.char {
	list := make([]string, 0, int(count))
	for _, p := range unsafe.Slice(paths, int(count)) {
		list = append(list, C.GoString(p))
	}
	return C.CString("window.__icebathDroppedFiles && window.__icebathDroppedFiles(" + droppedFilesJSON(list) + ")")
}

// Lets WebKit take OS file drops as DOM events and hands the bridge each dragged file's path first
func nativeInstallFileDrop(w *application.WebviewWindow) {
	application.InvokeSync(func() {
		if p := w.NativeWindow(); p != nil {
			C.icebath_install_file_drop(p, C.guintptr(w.ID()))
		}
	})
}

// The GTK3 chooser Wails opens never asks before replacing a file, so the shell asks
func nativeSaveNeedsOverwritePrompt() bool { return true }
