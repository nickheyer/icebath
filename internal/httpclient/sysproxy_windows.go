package httpclient

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	winhttpAccessNoProxy    = 1
	winhttpAccessNamedProxy = 3
	winhttpAutoDetect       = 0x1
	winhttpConfigURL        = 0x2
	winhttpDetectDHCP       = 0x1
	winhttpDetectDNS        = 0x2
)

var (
	winhttp           = windows.NewLazySystemDLL("winhttp.dll")
	procIEProxyConfig = winhttp.NewProc("WinHttpGetIEProxyConfigForCurrentUser")
	procWinHTTPOpen   = winhttp.NewProc("WinHttpOpen")
	procProxyForURL   = winhttp.NewProc("WinHttpGetProxyForUrl")
	procWinHTTPClose  = winhttp.NewProc("WinHttpCloseHandle")
	procGlobalFree    = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalFree")
)

// Internet settings of the current user as WinHTTP reads them
type ieProxyConfig struct {
	autoDetect    int32
	autoConfigURL *uint16
	proxy         *uint16
	proxyBypass   *uint16
}

// Auto proxy request asking for WPAD discovery, a PAC URL, or both
type autoproxyOptions struct {
	flags                 uint32
	autoDetectFlags       uint32
	autoConfigURL         *uint16
	reserved              uintptr
	reservedFlags         uint32
	autoLogonIfChallenged int32
}

// Proxy answer of a WinHTTP auto proxy evaluation
type winProxyInfo struct {
	accessType  uint32
	proxy       *uint16
	proxyBypass *uint16
}

// Proxies the current user's Internet settings give a target, WPAD and PAC evaluated before the manual list
func osProxies(ctx context.Context, target *url.URL) ([]*url.URL, error) {
	for _, proc := range []*windows.LazyProc{procIEProxyConfig, procWinHTTPOpen, procProxyForURL, procWinHTTPClose, procGlobalFree} {
		if err := proc.Find(); err != nil {
			return nil, err
		}
	}
	var config ieProxyConfig
	if ok, _, err := procIEProxyConfig.Call(uintptr(unsafe.Pointer(&config))); ok == 0 {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, nil
		}
		return nil, err
	}
	autoConfigURL := takeWinString(config.autoConfigURL)
	manual := takeWinString(config.proxy)
	bypass := takeWinString(config.proxyBypass)
	if config.autoDetect != 0 || autoConfigURL != "" {
		if routes, ok := autoProxy(ctx, target, config.autoDetect != 0, autoConfigURL); ok {
			return routes, nil
		}
	}
	if manual == "" || bypassedWindows(bypass, target) {
		return nil, nil
	}
	return windowsEntries(manual, strings.ToLower(target.Scheme)), nil
}

// Text of a WinHTTP allocated string, freed after reading
func takeWinString(p *uint16) string {
	if p == nil {
		return ""
	}
	text := windows.UTF16PtrToString(p)
	procGlobalFree.Call(uintptr(unsafe.Pointer(p)))
	return text
}

// WPAD or PAC answer for a target, false when discovery or the script failed so manual settings apply
func autoProxy(ctx context.Context, target *url.URL, detect bool, configURL string) ([]*url.URL, bool) {
	type answer struct {
		routes []*url.URL
		ok     bool
	}
	done := make(chan answer, 1)
	go func() {
		routes, ok := winAutoProxy(target, detect, configURL)
		done <- answer{routes, ok}
	}()
	select {
	case a := <-done:
		return a.routes, a.ok
	case <-ctx.Done():
		return nil, false
	}
}

// Blocking WinHTTP auto proxy evaluation for one target
func winAutoProxy(target *url.URL, detect bool, configURL string) ([]*url.URL, bool) {
	agent, _ := windows.UTF16PtrFromString(Product)
	session, _, _ := procWinHTTPOpen.Call(uintptr(unsafe.Pointer(agent)), winhttpAccessNoProxy, 0, 0, 0)
	if session == 0 {
		return nil, false
	}
	defer procWinHTTPClose.Call(session)
	options := autoproxyOptions{autoLogonIfChallenged: 1}
	if detect {
		options.flags |= winhttpAutoDetect
		options.autoDetectFlags = winhttpDetectDHCP | winhttpDetectDNS
	}
	if configURL != "" {
		options.flags |= winhttpConfigURL
		options.autoConfigURL, _ = windows.UTF16PtrFromString(configURL)
	}
	address, err := windows.UTF16PtrFromString(target.String())
	if err != nil {
		return nil, false
	}
	var info winProxyInfo
	if ok, _, _ := procProxyForURL.Call(session, uintptr(unsafe.Pointer(address)), uintptr(unsafe.Pointer(&options)), uintptr(unsafe.Pointer(&info))); ok == 0 {
		return nil, false
	}
	list := takeWinString(info.proxy)
	bypass := takeWinString(info.proxyBypass)
	if info.accessType != winhttpAccessNamedProxy || list == "" || bypassedWindows(bypass, target) {
		return nil, true
	}
	return windowsEntries(list, strings.ToLower(target.Scheme)), true
}

// Routes of a WinINet proxy list for a scheme, scheme scoped entries first, then bare ones, then SOCKS
func windowsEntries(list, scheme string) []*url.URL {
	var scoped, bare, socks []*url.URL
	for _, item := range strings.FieldsFunc(list, func(r rune) bool { return r == ';' || r == ' ' || r == '\t' }) {
		name, value, isScoped := strings.Cut(item, "=")
		switch {
		case !isScoped:
			bare = appendRoute(bare, winRoute(item, "http"))
		case strings.EqualFold(name, scheme):
			scoped = appendRoute(scoped, winRoute(value, "http"))
		case strings.EqualFold(name, "socks"):
			socks = appendRoute(socks, winRoute(value, "socks4"))
		}
	}
	switch {
	case len(scoped) > 0:
		return scoped
	case len(bare) > 0:
		return bare
	}
	return socks
}

func appendRoute(routes []*url.URL, r *url.URL) []*url.URL {
	if r == nil {
		return routes
	}
	return append(routes, r)
}

// Route of one WinINet entry, its scheme prefix honoured and SOCKS meaning SOCKS4 as Windows defines it
func winRoute(entry, scheme string) *url.URL {
	if prefix, rest, ok := strings.Cut(entry, "://"); ok {
		switch p := strings.ToLower(prefix); p {
		case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
			scheme = p
		case "socks":
			scheme = "socks4"
		default:
			return nil
		}
		entry = rest
	}
	entry = strings.TrimSuffix(entry, "/")
	if _, _, err := net.SplitHostPort(entry); err != nil {
		port := "80"
		switch scheme {
		case "https":
			port = "443"
		case "socks4", "socks4a", "socks5", "socks5h":
			port = "1080"
		}
		entry = net.JoinHostPort(strings.Trim(entry, "[]"), port)
	}
	return osRoute(scheme, entry)
}

// Reports whether a WinINet bypass list exempts the target, local meaning a host name without dots
func bypassedWindows(list string, target *url.URL) bool {
	host, port := targetHostPort(target)
	scheme := strings.ToLower(target.Scheme)
	for _, rule := range strings.FieldsFunc(list, func(r rune) bool { return r == ';' || r == ' ' || r == '\t' || r == ',' }) {
		rule = strings.ToLower(strings.TrimSpace(rule))
		switch rule {
		case "<local>":
			if !strings.Contains(host, ".") && !strings.Contains(host, ":") {
				return true
			}
			continue
		case "<-loopback>":
			continue
		}
		if prefix, rest, ok := strings.Cut(rule, "://"); ok {
			if prefix != scheme {
				continue
			}
			rule = rest
		}
		pattern := rule
		if h, p, err := net.SplitHostPort(rule); err == nil {
			if n, err := strconv.Atoi(p); err != nil || n != port {
				continue
			}
			pattern = h
		}
		if globMatch(strings.Trim(pattern, "[]"), host) {
			return true
		}
	}
	return false
}

// Reports whether text matches a pattern where star matches any run and question mark one character
func globMatch(pattern, text string) bool {
	p, t, star, resume := 0, 0, -1, 0
	for t < len(text) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == text[t]):
			p, t = p+1, t+1
		case p < len(pattern) && pattern[p] == '*':
			star, resume = p, t
			p++
		case star >= 0:
			resume++
			p, t = star+1, resume
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
