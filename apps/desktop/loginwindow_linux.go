//go:build linux

package main

/*
#cgo pkg-config: gtk+-3.0 webkit2gtk-4.1 libsoup-3.0
#include <gtk/gtk.h>
#include <webkit2/webkit2.h>
#include <libsoup/soup.h>
#include <stdlib.h>
#include <string.h>

extern void icebathLoginEvent(uintptr_t handle, int kind, char *uri, int code, char *text);
extern void icebathLoginCookies(uintptr_t token, GList *cookies, char *error);
extern void icebathLoginReplaced(uintptr_t token, char *error);
extern void icebathLoginContextInit(uintptr_t handle, WebKitWebContext *ctx);

enum { EV_COMMITTED = 1, EV_REDIRECTED = 2, EV_FINISHED = 3, EV_FAILED = 4, EV_GONE = 5, EV_CLOSE = 6, EV_DESTROYED = 7 };

static void icebath_init_extensions(WebKitWebContext *ctx, gpointer data) {
	icebathLoginContextInit((uintptr_t)data, ctx);
}

static void icebath_set_extension_data(WebKitWebContext *ctx, const char *socket) {
	webkit_web_context_set_web_extensions_initialization_user_data(ctx, g_variant_new_string(socket));
}

static WebKitWebContext *icebath_login_context(const char *data_dir, const char *cache_dir, const char *cookie_file, const char *ext_dir, uintptr_t handle) {
	WebKitWebsiteDataManager *manager = webkit_website_data_manager_new("base-data-directory", data_dir, "base-cache-directory", cache_dir, NULL);
	WebKitWebContext *ctx = webkit_web_context_new_with_website_data_manager(manager);
	WebKitCookieManager *cookies = webkit_website_data_manager_get_cookie_manager(manager);
	webkit_cookie_manager_set_persistent_storage(cookies, cookie_file, WEBKIT_COOKIE_PERSISTENT_STORAGE_SQLITE);
	webkit_cookie_manager_set_accept_policy(cookies, WEBKIT_COOKIE_POLICY_ACCEPT_ALWAYS);
	webkit_web_context_set_web_extensions_directory(ctx, ext_dir);
	g_signal_connect(ctx, "initialize-web-extensions", G_CALLBACK(icebath_init_extensions), (gpointer)handle);
	g_object_unref(manager);
	return ctx;
}

static void icebath_load_changed(WebKitWebView *view, WebKitLoadEvent event, gpointer data) {
	const char *uri = webkit_web_view_get_uri(view);
	int kind = event == WEBKIT_LOAD_COMMITTED ? EV_COMMITTED : event == WEBKIT_LOAD_REDIRECTED ? EV_REDIRECTED : event == WEBKIT_LOAD_FINISHED ? EV_FINISHED : 0;
	if (kind) {
		icebathLoginEvent((uintptr_t)data, kind, (char *)(uri ? uri : ""), 0, "");
	}
}

static gboolean icebath_load_failed(WebKitWebView *view, WebKitLoadEvent event, gchar *uri, GError *error, gpointer data) {
	int code = error && error->domain == WEBKIT_NETWORK_ERROR && error->code == WEBKIT_NETWORK_ERROR_CANCELLED ? -3 : (error ? error->code : -2);
	icebathLoginEvent((uintptr_t)data, EV_FAILED, uri ? uri : "", code, error ? error->message : "");
	return FALSE;
}

static void icebath_process_gone(WebKitWebView *view, WebKitWebProcessTerminationReason reason, gpointer data) {
	const char *text = reason == WEBKIT_WEB_PROCESS_CRASHED ? "crashed" : reason == WEBKIT_WEB_PROCESS_EXCEEDED_MEMORY_LIMIT ? "oom" : "killed";
	icebathLoginEvent((uintptr_t)data, EV_GONE, "", 0, (char *)text);
}

static gboolean icebath_delete(GtkWidget *w, GdkEvent *e, gpointer data) {
	icebathLoginEvent((uintptr_t)data, EV_CLOSE, "", 0, "");
	return TRUE;
}

static void icebath_destroyed(GtkWidget *w, gpointer data) {
	icebathLoginEvent((uintptr_t)data, EV_DESTROYED, "", 0, "");
}

static GtkWidget *icebath_login_window(WebKitWebContext *ctx, const char *title, int width, int height, uintptr_t handle, WebKitWebView **out) {
	GtkWidget *win = gtk_window_new(GTK_WINDOW_TOPLEVEL);
	gtk_window_set_title(GTK_WINDOW(win), title);
	gtk_window_set_default_size(GTK_WINDOW(win), width, height);
	GtkWidget *view = webkit_web_view_new_with_context(ctx);
	gtk_container_add(GTK_CONTAINER(win), view);
	g_signal_connect(view, "load-changed", G_CALLBACK(icebath_load_changed), (gpointer)handle);
	g_signal_connect(view, "load-failed", G_CALLBACK(icebath_load_failed), (gpointer)handle);
	g_signal_connect(view, "web-process-terminated", G_CALLBACK(icebath_process_gone), (gpointer)handle);
	g_signal_connect(win, "delete-event", G_CALLBACK(icebath_delete), (gpointer)handle);
	g_signal_connect(win, "destroy", G_CALLBACK(icebath_destroyed), (gpointer)handle);
	gtk_widget_show(view);
	*out = WEBKIT_WEB_VIEW(view);
	return win;
}

static void icebath_load(WebKitWebView *view, const char *uri, char **names, char **values, int count) {
	WebKitURIRequest *request = webkit_uri_request_new(uri);
	SoupMessageHeaders *headers = webkit_uri_request_get_http_headers(request);
	for (int i = 0; headers && i < count; i++) {
		soup_message_headers_replace(headers, names[i], values[i]);
	}
	webkit_web_view_load_request(view, request);
	g_object_unref(request);
}

static void icebath_cookies_ready(GObject *source, GAsyncResult *result, gpointer token) {
	GError *error = NULL;
	GList *cookies = webkit_cookie_manager_get_all_cookies_finish(WEBKIT_COOKIE_MANAGER(source), result, &error);
	icebathLoginCookies((uintptr_t)token, cookies, error ? error->message : NULL);
	g_list_free_full(cookies, (GDestroyNotify)soup_cookie_free);
	if (error) {
		g_error_free(error);
	}
}

static void icebath_get_cookies(WebKitWebContext *ctx, uintptr_t token) {
	WebKitCookieManager *cookies = webkit_website_data_manager_get_cookie_manager(webkit_web_context_get_website_data_manager(ctx));
	webkit_cookie_manager_get_all_cookies(cookies, NULL, icebath_cookies_ready, (gpointer)token);
}

static void icebath_replaced(GObject *source, GAsyncResult *result, gpointer token) {
	GError *error = NULL;
	webkit_cookie_manager_replace_cookies_finish(WEBKIT_COOKIE_MANAGER(source), result, &error);
	icebathLoginReplaced((uintptr_t)token, error ? error->message : NULL);
	if (error) {
		g_error_free(error);
	}
}

static GList *icebath_cookie_prepend(GList *list, const char *name, const char *value, const char *domain, const char *path, gint64 expires, gboolean secure, gboolean http_only, int same_site) {
	SoupCookie *cookie = soup_cookie_new(name, value, domain, path, -1);
	if (expires > 0) {
		GDateTime *at = g_date_time_new_from_unix_utc(expires);
		soup_cookie_set_expires(cookie, at);
		g_date_time_unref(at);
	}
	soup_cookie_set_secure(cookie, secure);
	soup_cookie_set_http_only(cookie, http_only);
	soup_cookie_set_same_site_policy(cookie, (SoupSameSitePolicy)same_site);
	return g_list_prepend(list, cookie);
}

static void icebath_cleared(GObject *source, GAsyncResult *result, gpointer token) {
	GError *error = NULL;
	webkit_website_data_manager_clear_finish(WEBKIT_WEBSITE_DATA_MANAGER(source), result, &error);
	icebathLoginReplaced((uintptr_t)token, error ? error->message : NULL);
	if (error) {
		g_error_free(error);
	}
}

static void icebath_replace_cookies(WebKitWebContext *ctx, GList *list, uintptr_t token) {
	WebKitWebsiteDataManager *manager = webkit_web_context_get_website_data_manager(ctx);
	if (!list) {
		webkit_website_data_manager_clear(manager, WEBKIT_WEBSITE_DATA_COOKIES, 0, NULL, icebath_cleared, (gpointer)token);
		return;
	}
	webkit_cookie_manager_replace_cookies(webkit_website_data_manager_get_cookie_manager(manager), list, NULL, icebath_replaced, (gpointer)token);
	g_list_free_full(list, (GDestroyNotify)soup_cookie_free);
}

static const char *icebath_cookie_name(GList *l) { return soup_cookie_get_name((SoupCookie *)l->data); }
static const char *icebath_cookie_value(GList *l) { return soup_cookie_get_value((SoupCookie *)l->data); }
static const char *icebath_cookie_domain(GList *l) { return soup_cookie_get_domain((SoupCookie *)l->data); }
static const char *icebath_cookie_path(GList *l) { return soup_cookie_get_path((SoupCookie *)l->data); }
static gboolean icebath_cookie_secure(GList *l) { return soup_cookie_get_secure((SoupCookie *)l->data); }
static gboolean icebath_cookie_http_only(GList *l) { return soup_cookie_get_http_only((SoupCookie *)l->data); }
static int icebath_cookie_same_site(GList *l) { return (int)soup_cookie_get_same_site_policy((SoupCookie *)l->data); }
static gint64 icebath_cookie_expires(GList *l) {
	GDateTime *at = soup_cookie_get_expires((SoupCookie *)l->data);
	return at ? g_date_time_to_unix(at) : 0;
}
static void icebath_window_show(GtkWidget *w) { gtk_widget_show(w); gtk_window_present(GTK_WINDOW(w)); }
static void icebath_window_focus(GtkWidget *w) { gtk_window_present(GTK_WINDOW(w)); }
static gboolean icebath_window_visible(GtkWidget *w) { return gtk_widget_get_visible(w); }
static void icebath_window_destroy(GtkWidget *w) { gtk_widget_destroy(w); }
static guint64 icebath_page_id(WebKitWebView *view) { return webkit_web_view_get_page_id(view); }
*/
import "C"

import (
	"errors"
	"os"
	"path/filepath"
	"runtime/cgo"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/nickheyer/icebath/pkg/desktop/connections"
)

// Per partition WebKit contexts, each with its own persistent cookie store like an Electron partition
type loginContexts struct {
	mu     sync.Mutex
	byName map[string]*loginContext
	socket *loginHeaderServer
}

type loginContext struct {
	ctx    *C.WebKitWebContext
	handle cgo.Handle
	socket string
}

var loginState = loginContexts{byName: map[string]*loginContext{}}

// Directory of the sign-in web process extension beside the executable
func loginExtensionDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(exe), "webkit-extensions")
	if _, err := os.Stat(filepath.Join(dir, "libicebath-login.so")); err != nil {
		return "", errors.New("Sign-in web extension is missing at " + filepath.Join(dir, "libicebath-login.so"))
	}
	return dir, nil
}

//export icebathLoginContextInit
func icebathLoginContextInit(handle C.uintptr_t, ctx *C.WebKitWebContext) {
	lc := cgo.Handle(handle).Value().(*loginContext)
	socket := C.CString(lc.socket)
	defer C.free(unsafe.Pointer(socket))
	C.icebath_set_extension_data(ctx, socket)
}

// WebKit context of a partition, created on the main thread on first use
func (h *authHostShell) loginContext(partition string) (*loginContext, error) {
	loginState.mu.Lock()
	defer loginState.mu.Unlock()
	if lc := loginState.byName[partition]; lc != nil {
		return lc, nil
	}
	ext, err := loginExtensionDir()
	if err != nil {
		return nil, err
	}
	if loginState.socket == nil {
		server, err := startLoginHeaderServer(h.shell.userData)
		if err != nil {
			return nil, err
		}
		loginState.socket = server
	}
	base := filepath.Join(h.shell.userData, "Partitions", strings.TrimPrefix(partition, "persist:"), "webkit")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	lc := &loginContext{socket: loginState.socket.path}
	lc.handle = cgo.NewHandle(lc)
	data, cache, cookies, extDir := C.CString(filepath.Join(base, "data")), C.CString(filepath.Join(base, "cache")), C.CString(filepath.Join(base, "cookies.sqlite")), C.CString(ext)
	defer func() {
		for _, p := range []*C.char{data, cache, cookies, extDir} {
			C.free(unsafe.Pointer(p))
		}
	}()
	application.InvokeSync(func() { lc.ctx = C.icebath_login_context(data, cache, cookies, extDir, C.uintptr_t(lc.handle)) })
	loginState.byName[partition] = lc
	return lc, nil
}

type pendingCookies struct {
	done    chan struct{}
	cookies []connections.JarCookie
	err     error
}

var cookieRequests sync.Map

//export icebathLoginCookies
func icebathLoginCookies(token C.uintptr_t, list *C.GList, cerr *C.char) {
	value, ok := cookieRequests.LoadAndDelete(uintptr(token))
	if !ok {
		return
	}
	p := value.(*pendingCookies)
	if cerr != nil {
		p.err = errors.New(C.GoString(cerr))
	}
	for l := list; l != nil; l = l.next {
		domain := C.GoString(C.icebath_cookie_domain(l))
		c := connections.JarCookie{Name: C.GoString(C.icebath_cookie_name(l)), Value: C.GoString(C.icebath_cookie_value(l)),
			Domain: strings.TrimPrefix(domain, "."), HostOnly: !strings.HasPrefix(domain, "."), Path: C.GoString(C.icebath_cookie_path(l)),
			Secure: C.icebath_cookie_secure(l) != 0, HTTPOnly: C.icebath_cookie_http_only(l) != 0, Session: true}
		switch C.icebath_cookie_same_site(l) {
		case C.SOUP_SAME_SITE_POLICY_LAX:
			c.SameSite = "lax"
		case C.SOUP_SAME_SITE_POLICY_STRICT:
			c.SameSite = "strict"
		default:
			c.SameSite = "no_restriction"
		}
		if at := int64(C.icebath_cookie_expires(l)); at > 0 {
			c.Session, c.Expires = false, time.Unix(at, 0)
		}
		p.cookies = append(p.cookies, c)
	}
	close(p.done)
}

//export icebathLoginReplaced
func icebathLoginReplaced(token C.uintptr_t, cerr *C.char) {
	value, ok := cookieRequests.LoadAndDelete(uintptr(token))
	if !ok {
		return
	}
	p := value.(*pendingCookies)
	if cerr != nil {
		p.err = errors.New(C.GoString(cerr))
	}
	close(p.done)
}

var cookieTokens atomic.Uintptr

// Every cookie of a partition store, read on the main loop and awaited off it
func (lc *loginContext) cookies() ([]connections.JarCookie, error) {
	token := cookieTokens.Add(1)
	p := &pendingCookies{done: make(chan struct{})}
	cookieRequests.Store(token, p)
	application.InvokeAsync(func() { C.icebath_get_cookies(lc.ctx, C.uintptr_t(token)) })
	<-p.done
	return p.cookies, p.err
}

// Replaces every cookie of a partition store with the jar's cookies
func (lc *loginContext) replace(cookies []connections.JarCookie) error {
	token := cookieTokens.Add(1)
	p := &pendingCookies{done: make(chan struct{})}
	cookieRequests.Store(token, p)
	application.InvokeAsync(func() {
		var list *C.GList
		for _, c := range cookies {
			domain := c.Domain
			if !c.HostOnly {
				domain = "." + domain
			}
			var expires int64
			if !c.Session {
				expires = c.Expires.Unix()
			}
			sameSite := C.SOUP_SAME_SITE_POLICY_NONE
			switch c.SameSite {
			case "lax":
				sameSite = C.SOUP_SAME_SITE_POLICY_LAX
			case "strict":
				sameSite = C.SOUP_SAME_SITE_POLICY_STRICT
			}
			name, value, d, path := C.CString(c.Name), C.CString(c.Value), C.CString(domain), C.CString(c.Path)
			list = C.icebath_cookie_prepend(list, name, value, d, path, C.gint64(expires), cbool(c.Secure), cbool(c.HTTPOnly), C.int(sameSite))
			for _, s := range []*C.char{name, value, d, path} {
				C.free(unsafe.Pointer(s))
			}
		}
		C.icebath_replace_cookies(lc.ctx, list, C.uintptr_t(token))
	})
	<-p.done
	return p.err
}

func cbool(b bool) C.gboolean {
	if b {
		return 1
	}
	return 0
}

// gtkLoginWindow is a GTK sign-in window over its partition's WebKit context
type gtkLoginWindow struct {
	spec      connections.LoginWindowSpec
	lc        *loginContext
	handle    cgo.Handle
	win       *C.GtkWidget
	view      *C.WebKitWebView
	destroyed atomic.Bool
	closing   atomic.Bool
	mu        sync.Mutex
	loadDone  func(error)
	pageID    uint64
}

func (h *authHostShell) openPlatformLoginWindow(spec connections.LoginWindowSpec) (connections.LoginWindow, error) {
	lc, err := h.loginContext(spec.Partition)
	if err != nil {
		return nil, err
	}
	if err := lc.replace(spec.Cookies); err != nil {
		return nil, err
	}
	w := &gtkLoginWindow{spec: spec, lc: lc}
	w.handle = cgo.NewHandle(w)
	title := C.CString(spec.Title)
	defer C.free(unsafe.Pointer(title))
	application.InvokeSync(func() {
		w.win = C.icebath_login_window(lc.ctx, title, C.int(spec.Width), C.int(spec.Height), C.uintptr_t(w.handle), &w.view)
		w.pageID = uint64(C.icebath_page_id(w.view))
		loginState.socket.register(w.pageID, spec.Headers)
		if spec.Show {
			C.icebath_window_show(w.win)
		}
	})
	return w, nil
}

//export icebathLoginEvent
func icebathLoginEvent(handle C.uintptr_t, kind C.int, curi *C.char, code C.int, ctext *C.char) {
	w := cgo.Handle(handle).Value().(*gtkLoginWindow)
	uri, text := C.GoString(curi), C.GoString(ctext)
	switch kind {
	case C.EV_COMMITTED:
		if w.spec.OnNavigate != nil {
			go w.spec.OnNavigate(uri)
		}
	case C.EV_REDIRECTED:
		if w.spec.OnRedirect != nil {
			go w.spec.OnRedirect(uri)
		}
	case C.EV_FINISHED:
		w.settleLoad(nil)
	case C.EV_FAILED:
		if w.spec.OnFailLoad != nil {
			w.spec.OnFailLoad(strconv.Itoa(int(code)), uri)
		}
		w.settleLoad(&connections.LoginLoadError{Code: int(code), Message: text})
	case C.EV_GONE:
		if w.spec.OnRendererGone != nil {
			go w.spec.OnRendererGone(text, "")
		}
	case C.EV_CLOSE:
		if w.closing.Swap(true) {
			return
		}
		go func() {
			final, err := w.lc.cookies()
			if err != nil {
				final = nil
			}
			w.Destroy()
			if w.spec.OnClosed != nil {
				w.spec.OnClosed(final)
			}
		}()
	case C.EV_DESTROYED:
		w.destroyed.Store(true)
		loginState.socket.unregister(w.pageID)
		w.settleLoad(&connections.LoginLoadError{Code: -3, Message: "ERR_ABORTED (-3) loading the sign-in page"})
		go w.handle.Delete()
	}
}

func (w *gtkLoginWindow) settleLoad(err error) {
	w.mu.Lock()
	done := w.loadDone
	w.loadDone = nil
	w.mu.Unlock()
	if done != nil {
		go done(err)
	}
}

// Load navigates with the extra headers on the first request
func (w *gtkLoginWindow) Load(url string, headers map[string]string, done func(error)) {
	w.mu.Lock()
	w.loadDone = done
	w.mu.Unlock()
	curl := C.CString(url)
	names := make([]*C.char, 0, len(headers))
	values := make([]*C.char, 0, len(headers))
	for name, value := range headers {
		names = append(names, C.CString(name))
		values = append(values, C.CString(value))
	}
	application.InvokeAsync(func() {
		defer C.free(unsafe.Pointer(curl))
		var np, vp **C.char
		if len(names) > 0 {
			np, vp = &names[0], &values[0]
		}
		if !w.destroyed.Load() {
			C.icebath_load(w.view, curl, np, vp, C.int(len(names)))
		}
		for i := range names {
			C.free(unsafe.Pointer(names[i]))
			C.free(unsafe.Pointer(values[i]))
		}
	})
}

func (w *gtkLoginWindow) Show() {
	application.InvokeAsync(func() {
		if !w.destroyed.Load() {
			C.icebath_window_show(w.win)
		}
	})
}

func (w *gtkLoginWindow) Focus() {
	application.InvokeAsync(func() {
		if !w.destroyed.Load() {
			C.icebath_window_focus(w.win)
		}
	})
}

func (w *gtkLoginWindow) IsVisible() bool {
	return application.InvokeSyncWithResult(func() bool { return !w.destroyed.Load() && C.icebath_window_visible(w.win) != 0 })
}

func (w *gtkLoginWindow) IsDestroyed() bool { return w.destroyed.Load() }

func (w *gtkLoginWindow) Destroy() {
	application.InvokeSync(func() {
		if !w.destroyed.Load() {
			C.icebath_window_destroy(w.win)
		}
	})
}

func (w *gtkLoginWindow) Cookies() ([]connections.JarCookie, error) { return w.lc.cookies() }
