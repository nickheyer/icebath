//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/w32"

	"github.com/nickheyer/icebath/pkg/desktop/connections"
)

const loginWindowClass = "IcebathSignInWindow"

var (
	loginClassOnce sync.Once
	loginWindows   sync.Map
)

// COM layouts of the vtable entries go-webview2 declares but does not expose
type comVtblHeaders struct {
	QueryInterface, AddRef, Release                                       uintptr
	GetHeader, GetHeaders, Contains, SetHeader, RemoveHeader, GetIterator uintptr
}

type comHeaders struct{ vtbl *comVtblHeaders }

type comVtblNavigationCompleted struct {
	QueryInterface, AddRef, Release                  uintptr
	GetIsSuccess, GetWebErrorStatus, GetNavigationId uintptr
}

type comNavigationCompleted struct{ vtbl *comVtblNavigationCompleted }

// Removes a request header through the vtable entry the binding leaves unexported
func removeRequestHeader(headers *edge.ICoreWebView2HttpRequestHeaders, name string) {
	h := (*comHeaders)(unsafe.Pointer(headers))
	ptr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return
	}
	_, _, _ = syscall.SyscallN(h.vtbl.RemoveHeader, uintptr(unsafe.Pointer(headers)), uintptr(unsafe.Pointer(ptr)))
}

// Success flag and web error status of a finished navigation
func navigationOutcome(args *edge.ICoreWebView2NavigationCompletedEventArgs) (bool, int32) {
	a := (*comNavigationCompleted)(unsafe.Pointer(args))
	var ok int32
	var status int32
	_, _, _ = syscall.SyscallN(a.vtbl.GetIsSuccess, uintptr(unsafe.Pointer(args)), uintptr(unsafe.Pointer(&ok)))
	_, _, _ = syscall.SyscallN(a.vtbl.GetWebErrorStatus, uintptr(unsafe.Pointer(args)), uintptr(unsafe.Pointer(&status)))
	return ok != 0, status
}

// webView2LoginWindow is a Win32 sign-in window hosting WebView2 over its partition's own user data folder
type webView2LoginWindow struct {
	spec      connections.LoginWindowSpec
	hwnd      w32.HWND
	chromium  *edge.Chromium
	destroyed atomic.Bool
	closing   atomic.Bool
	mu        sync.Mutex
	loadDone  func(error)
	firstURL  string
	firstHdrs map[string]string
	requestID atomic.Int64
}

func loginWndProc(hwnd w32.HWND, msg uint32, wparam, lparam uintptr) uintptr {
	value, ok := loginWindows.Load(hwnd)
	if !ok {
		return w32.DefWindowProc(hwnd, msg, wparam, lparam)
	}
	w := value.(*webView2LoginWindow)
	switch msg {
	case w32.WM_SIZE:
		if w.chromium != nil {
			w.chromium.Resize()
		}
	case w32.WM_CLOSE:
		if !w.closing.Swap(true) {
			go func() {
				final, err := w.Cookies()
				if err != nil {
					final = nil
				}
				w.Destroy()
				if w.spec.OnClosed != nil {
					w.spec.OnClosed(final)
				}
			}()
		}
		return 0
	case w32.WM_DESTROY:
		w.destroyed.Store(true)
		loginWindows.Delete(hwnd)
		w.settleLoad(&connections.LoginLoadError{Code: -3, Message: "ERR_ABORTED (-3) loading the sign-in page"})
	}
	return w32.DefWindowProc(hwnd, msg, wparam, lparam)
}

func registerLoginClass() {
	loginClassOnce.Do(func() {
		instance := w32.GetModuleHandle("")
		class := w32.WNDCLASSEX{
			WndProc:   syscall.NewCallback(loginWndProc),
			Instance:  instance,
			Cursor:    w32.LoadCursorWithResourceID(0, w32.IDC_ARROW),
			ClassName: w32.MustStringToUTF16Ptr(loginWindowClass),
		}
		class.Size = uint32(unsafe.Sizeof(class))
		w32.RegisterClassEx(&class)
	})
}

// User data folder of a partition, persistent per partition like an Electron session
func (h *authHostShell) loginDataPath(partition string) (string, error) {
	path := filepath.Join(h.shell.userData, "Partitions", strings.TrimPrefix(partition, "persist:"), "webview2")
	return path, os.MkdirAll(path, 0o700)
}

func (h *authHostShell) openPlatformLoginWindow(spec connections.LoginWindowSpec) (connections.LoginWindow, error) {
	dataPath, err := h.loginDataPath(spec.Partition)
	if err != nil {
		return nil, err
	}
	w := &webView2LoginWindow{spec: spec}
	var createErr error
	application.InvokeSync(func() {
		registerLoginClass()
		w.hwnd = w32.CreateWindowEx(0, w32.MustStringToUTF16Ptr(loginWindowClass), w32.MustStringToUTF16Ptr(spec.Title),
			w32.WS_OVERLAPPEDWINDOW, w32.CW_USEDEFAULT, w32.CW_USEDEFAULT, spec.Width, spec.Height, 0, 0, w32.GetModuleHandle(""), nil)
		if w.hwnd == 0 {
			createErr = errors.New("Could not create the sign-in window.")
			return
		}
		loginWindows.Store(w.hwnd, w)
		w.chromium = edge.NewChromium()
		w.chromium.DataPath = dataPath
		w.chromium.WebResourceRequestedCallback = w.onRequest
		w.chromium.NavigationCompletedCallback = w.onNavigationCompleted
		w.chromium.ProcessFailedCallback = w.onProcessFailed
		if !w.chromium.Embed(uintptr(w.hwnd)) {
			createErr = errors.New("Could not start WebView2 for the sign-in window.")
			return
		}
		w.chromium.AddWebResourceRequestedFilter("*", edge.COREWEBVIEW2_WEB_RESOURCE_CONTEXT_ALL)
		w.chromium.Resize()
		if spec.Show {
			w32.ShowWindow(w.hwnd, w32.SW_SHOW)
			w32.SetForegroundWindow(w.hwnd)
		}
	})
	if createErr != nil {
		if w.hwnd != 0 {
			w.Destroy()
		}
		return nil, createErr
	}
	if err := w.replaceCookies(spec.Cookies); err != nil {
		w.Destroy()
		return nil, err
	}
	return w, nil
}

// Applies the remote header rule to a request, the first load also carrying its extra headers
func (w *webView2LoginWindow) onRequest(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs) {
	uri, err := request.GetUri()
	if err != nil {
		return
	}
	headers, err := request.GetHeaders()
	if err != nil {
		return
	}
	current := map[string]string{}
	if it, err := headers.GetIterator(); err == nil {
		for {
			has, err := it.HasCurrentHeader()
			if err != nil || !has {
				break
			}
			name, value, err := it.GetCurrentHeader()
			if err == nil {
				current[name] = value
			}
			if more, err := it.MoveNext(); err != nil || !more {
				break
			}
		}
		it.Release()
	}
	w.mu.Lock()
	first := w.firstHdrs
	if first != nil && uri == w.firstURL {
		w.firstHdrs = nil
	} else {
		first = nil
	}
	w.mu.Unlock()
	id := int(w.requestID.Add(1))
	out := w.spec.Headers.Apply(connections.RemoteRequestDetails{ID: &id, URL: uri, RequestHeaders: current})
	w.spec.Headers.Forget(&id)
	if out == nil && first == nil {
		return
	}
	if out == nil {
		out = current
	}
	for name, value := range first {
		out[name] = value
	}
	for name := range current {
		if _, keep := out[name]; !keep {
			removeRequestHeader(headers, name)
		}
	}
	for name, value := range out {
		_ = headers.SetHeader(name, value)
	}
}

func (w *webView2LoginWindow) onNavigationCompleted(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs) {
	source, _ := sender.GetSource()
	ok, status := navigationOutcome(args)
	if ok {
		if w.spec.OnNavigate != nil {
			go w.spec.OnNavigate(source)
		}
		w.settleLoad(nil)
		return
	}
	code := -2
	if status == 14 {
		code = -3
	}
	if w.spec.OnFailLoad != nil {
		w.spec.OnFailLoad(strconv.Itoa(code), source)
	}
	w.settleLoad(&connections.LoginLoadError{Code: code, Message: "WebView2 navigation failed with status " + strconv.Itoa(int(status))})
}

func (w *webView2LoginWindow) onProcessFailed(sender *edge.ICoreWebView2, args *edge.ICoreWebView2ProcessFailedEventArgs) {
	kind, _ := args.GetProcessFailedKind()
	reason := "crashed"
	if kind == edge.COREWEBVIEW2_PROCESS_FAILED_KIND_RENDER_PROCESS_UNRESPONSIVE {
		if w.spec.OnUnresponsive != nil {
			go w.spec.OnUnresponsive()
		}
		return
	}
	if kind == edge.COREWEBVIEW2_PROCESS_FAILED_KIND_RENDER_PROCESS_EXITED {
		reason = "killed"
	}
	if w.spec.OnRendererGone != nil {
		go w.spec.OnRendererGone(reason, "")
	}
}

func (w *webView2LoginWindow) settleLoad(err error) {
	w.mu.Lock()
	done := w.loadDone
	w.loadDone = nil
	w.mu.Unlock()
	if done != nil {
		go done(err)
	}
}

func (w *webView2LoginWindow) Load(url string, headers map[string]string, done func(error)) {
	w.mu.Lock()
	w.loadDone, w.firstURL, w.firstHdrs = done, url, headers
	w.mu.Unlock()
	application.InvokeAsync(func() {
		if !w.destroyed.Load() {
			w.chromium.Navigate(url)
		}
	})
}

func (w *webView2LoginWindow) Show() {
	application.InvokeAsync(func() {
		if !w.destroyed.Load() {
			w32.ShowWindow(w.hwnd, w32.SW_SHOW)
			w32.SetForegroundWindow(w.hwnd)
		}
	})
}

func (w *webView2LoginWindow) Focus() {
	application.InvokeAsync(func() {
		if !w.destroyed.Load() {
			w32.SetForegroundWindow(w.hwnd)
			w.chromium.Focus()
		}
	})
}

func (w *webView2LoginWindow) IsVisible() bool {
	return application.InvokeSyncWithResult(func() bool { return !w.destroyed.Load() && w32.IsWindowVisible(w.hwnd) })
}

func (w *webView2LoginWindow) IsDestroyed() bool { return w.destroyed.Load() }

func (w *webView2LoginWindow) Destroy() {
	application.InvokeSync(func() {
		if !w.destroyed.Load() {
			if w.chromium != nil {
				w.chromium.ShuttingDown()
			}
			w32.DestroyWindow(w.hwnd)
		}
	})
}

// COM layout of ICoreWebView2CookieManager, whose GetCookies binding skips the completion handler
type comVtblCookieManager struct {
	QueryInterface, AddRef, Release                             uintptr
	CreateCookie, CopyCookie, GetCookies, AddOrUpdateCookie     uintptr
	DeleteCookie, DeleteCookies, DeleteCookiesWithDomainAndPath uintptr
	DeleteAllCookies                                            uintptr
}

type comCookieManager struct{ vtbl *comVtblCookieManager }

// getCookiesHandler is the ICoreWebView2GetCookiesCompletedHandler a cookie read waits on
type getCookiesHandler struct {
	vtbl *getCookiesHandlerVtbl
	refs int32
	done func(hr uintptr, list *edge.ICoreWebView2CookieList)
}

type getCookiesHandlerVtbl struct{ QueryInterface, AddRef, Release, Invoke uintptr }

var (
	liveCookieHandlers sync.Map
	getCookiesVtbl     = &getCookiesHandlerVtbl{
		QueryInterface: syscall.NewCallback(func(this *getCookiesHandler, riid uintptr, out *uintptr) uintptr {
			*out = uintptr(unsafe.Pointer(this))
			atomic.AddInt32(&this.refs, 1)
			return 0
		}),
		AddRef: syscall.NewCallback(func(this *getCookiesHandler) uintptr { return uintptr(atomic.AddInt32(&this.refs, 1)) }),
		Release: syscall.NewCallback(func(this *getCookiesHandler) uintptr {
			n := atomic.AddInt32(&this.refs, -1)
			if n <= 0 {
				liveCookieHandlers.Delete(this)
			}
			return uintptr(n)
		}),
		Invoke: syscall.NewCallback(func(this *getCookiesHandler, hr uintptr, list *edge.ICoreWebView2CookieList) uintptr {
			this.done(hr, list)
			return 0
		}),
	}
)

// Copies a cookie list into jar cookies
func jarCookiesFromList(list *edge.ICoreWebView2CookieList) ([]connections.JarCookie, error) {
	count, err := list.GetCount()
	if err != nil {
		return nil, err
	}
	out := make([]connections.JarCookie, 0, count)
	for i := uint32(0); i < count; i++ {
		cookie, err := list.GetItem(i)
		if err != nil {
			return nil, err
		}
		name, _ := cookie.GetName()
		value, _ := cookie.GetValue()
		domain, _ := cookie.GetDomain()
		path, _ := cookie.GetPath()
		expires, _ := cookie.GetExpires()
		httpOnly, _ := cookie.GetIsHttpOnly()
		secure, _ := cookie.GetIsSecure()
		sameSite, _ := cookie.GetSameSite()
		c := connections.JarCookie{Name: name, Value: value, Domain: strings.TrimPrefix(domain, "."), HostOnly: !strings.HasPrefix(domain, "."),
			Path: path, HTTPOnly: httpOnly, Secure: secure, Session: expires <= 0}
		switch sameSite {
		case 1:
			c.SameSite = "lax"
		case 2:
			c.SameSite = "strict"
		default:
			c.SameSite = "no_restriction"
		}
		if expires > 0 {
			c.Expires = time.Unix(0, int64(expires*float64(time.Second)))
		}
		cookie.Release()
		out = append(out, c)
	}
	return out, nil
}

// Every cookie of the window's user data folder, requested on the UI thread and awaited off it
func (w *webView2LoginWindow) Cookies() ([]connections.JarCookie, error) {
	type outcome struct {
		cookies []connections.JarCookie
		err     error
	}
	result := make(chan outcome, 1)
	application.InvokeAsync(func() {
		manager, err := w.chromium.GetCookieManager()
		if err != nil {
			result <- outcome{err: err}
			return
		}
		handler := &getCookiesHandler{vtbl: getCookiesVtbl, refs: 1}
		handler.done = func(hr uintptr, list *edge.ICoreWebView2CookieList) {
			defer manager.Release()
			if hr != 0 || list == nil {
				result <- outcome{err: syscall.Errno(hr)}
				return
			}
			cookies, err := jarCookiesFromList(list)
			result <- outcome{cookies: cookies, err: err}
		}
		liveCookieHandlers.Store(handler, true)
		empty, _ := syscall.UTF16PtrFromString("")
		m := (*comCookieManager)(unsafe.Pointer(manager))
		hr, _, _ := syscall.SyscallN(m.vtbl.GetCookies, uintptr(unsafe.Pointer(manager)), uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(handler)))
		if hr != 0 {
			liveCookieHandlers.Delete(handler)
			manager.Release()
			result <- outcome{err: syscall.Errno(hr)}
		}
	})
	o := <-result
	return o.cookies, o.err
}

// Replaces every cookie of the window's user data folder with the jar's cookies
func (w *webView2LoginWindow) replaceCookies(cookies []connections.JarCookie) error {
	return application.InvokeSyncWithError(func() error {
		manager, err := w.chromium.GetCookieManager()
		if err != nil {
			return err
		}
		defer manager.Release()
		if err := manager.DeleteAllCookies(); err != nil {
			return err
		}
		for _, c := range cookies {
			domain := c.Domain
			if !c.HostOnly {
				domain = "." + domain
			}
			cookie, err := manager.CreateCookie(c.Name, c.Value, domain, c.Path)
			if err != nil {
				return err
			}
			if !c.Session {
				_ = cookie.PutExpires(float64(c.Expires.UnixNano()) / float64(time.Second))
			}
			_ = cookie.PutIsHttpOnly(c.HTTPOnly)
			_ = cookie.PutIsSecure(c.Secure)
			switch c.SameSite {
			case "lax":
				_ = cookie.PutSameSite(1)
			case "strict":
				_ = cookie.PutSameSite(2)
			default:
				_ = cookie.PutSameSite(0)
			}
			err = manager.AddOrUpdateCookie(cookie)
			cookie.Release()
			if err != nil {
				return err
			}
		}
		return nil
	})
}
