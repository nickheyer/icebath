package httpclient

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/internal/dl"
)

// GIO proxy resolver entry points, the resolver GNOME and libproxy desktops configure
type gioBinding struct {
	err        error
	getDefault func() unsafe.Pointer
	lookup     func(resolver unsafe.Pointer, uri string, cancellable unsafe.Pointer, gerr *unsafe.Pointer) unsafe.Pointer
	strfreev   func(list unsafe.Pointer)
	errorFree  func(gerr unsafe.Pointer)
}

var (
	gioOnce sync.Once
	gio     gioBinding
)

// Error text that marks a missing GIO library, a desktop without any proxy configuration service
var errNoGIO = errors.New("libgio-2.0 is not installed")

// Bound resolver, loading libgio on first use
func loadGIO() *gioBinding {
	gioOnce.Do(func() {
		lib, err := dl.Open("libgio-2.0.so.0", dl.Global)
		if err != nil {
			gio.err = errNoGIO
			return
		}
		gio.err = lib.BindAll([]dl.Binding{
			{Fn: &gio.getDefault, Name: "g_proxy_resolver_get_default"},
			{Fn: &gio.lookup, Name: "g_proxy_resolver_lookup"},
			{Fn: &gio.strfreev, Name: "g_strfreev"},
			{Fn: &gio.errorFree, Name: "g_error_free"},
		})
	})
	return &gio
}

// Proxies the desktop proxy settings give a target through the default GIO resolver
func osProxies(ctx context.Context, target *url.URL) ([]*url.URL, error) {
	g := loadGIO()
	if errors.Is(g.err, errNoGIO) {
		return nil, nil
	}
	if g.err != nil {
		return nil, g.err
	}
	type answer struct {
		routes []*url.URL
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		routes, err := g.resolve(target.String())
		done <- answer{routes, err}
	}()
	select {
	case a := <-done:
		return a.routes, a.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Blocking lookup mapping each GIO proxy URI to a route, direct entries kept as nil
func (g *gioBinding) resolve(target string) ([]*url.URL, error) {
	resolver := g.getDefault()
	if resolver == nil {
		return nil, nil
	}
	var gerr unsafe.Pointer
	list := g.lookup(resolver, target, nil, &gerr)
	if list == nil {
		message := "GIO proxy lookup failed"
		if gerr != nil {
			message = cString(*(*unsafe.Pointer)(unsafe.Add(gerr, 8)))
			g.errorFree(gerr)
		}
		return nil, errors.New(message)
	}
	defer g.strfreev(list)
	var routes []*url.URL
	for i := 0; ; i++ {
		item := *(*unsafe.Pointer)(unsafe.Add(list, i*int(unsafe.Sizeof(uintptr(0)))))
		if item == nil {
			break
		}
		uri := cString(item)
		if uri == "direct://" {
			routes = append(routes, nil)
			continue
		}
		if r := gioRoute(uri); r != nil {
			routes = append(routes, r)
		}
	}
	return routes, nil
}

// Route of one GIO proxy URI with any credentials the desktop settings carry, nil for schemes without a dialer
func gioRoute(uri string) *url.URL {
	u, err := url.Parse(uri)
	if err != nil {
		return nil
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "socks":
		scheme = "socks5"
	case "http", "https", "socks4", "socks4a", "socks5":
	default:
		return nil
	}
	r := osRoute(scheme, u.Host)
	if r != nil {
		r.User = u.User
	}
	return r
}

// Copy of a NUL terminated C string
func cString(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}
