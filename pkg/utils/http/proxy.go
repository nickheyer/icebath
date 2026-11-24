package httputil

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// Proxy for a request: the http_proxy, https_proxy and all_proxy variables when any is set, else the operating system settings; nil for a host NO_PROXY, the system bypass list or a loopback address exempts, an error for a proxy setting that cannot be used
func Proxy(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	return currentProxies().proxyFor(req.URL)
}

// Scheme to proxy URL map with keys http and https, from the environment when it sets any proxy, else from the operating system
func Proxies() map[string]string { return currentProxies().table() }

// Whether the bypass list exempts the host, given with or without a port: NO_PROXY, the operating system list when the environment sets no proxy, and loopback hosts always
func ProxyBypass(host string) bool { return currentProxies().bypasses(hostURL(host)) }

// Scheme to proxy URL map the environment alone sets, empty when it sets no proxy
func EnvProxies() map[string]string {
	key, ok := envProxyKey()
	if !ok {
		return map[string]string{}
	}
	return settingsFor(key).table()
}

// Scheme to proxy URL map the operating system settings hold, empty on a platform that keeps none
func SystemProxies() map[string]string { return maps.Clone(systemSnapshot().proxies) }

// Whether the operating system bypass list exempts the host, given with or without a port
func SystemProxyBypass(host string) bool { return systemSnapshot().bypasses(hostURL(host).Hostname()) }

// Whether a NO_PROXY style list exempts the host, given with or without a port: comma separated names matching themselves and their subdomains, a leading dot or *. matching subdomains only, IP addresses and CIDR ranges, each with an optional port, * matching every host, loopback hosts always exempt
func NoProxyBypass(noProxy, host string) bool { return noProxyMatcher(noProxy)(hostURL(host)) }

// Proxy configuration one decision is made from
type proxyKey struct {
	http, https string
	noProxy     string
	// The http proxy came from http_proxy or HTTP_PROXY in a CGI environment, where a request header can set it
	cgiHTTP bool
	// Snapshot the proxies came from, nil when they came from the environment
	system *systemSettings
}

// Parsed form of one proxyKey
type proxySettings struct {
	key               proxyKey
	http, https       *url.URL
	httpErr, httpsErr error
	noProxy           func(*url.URL) bool
}

var lastProxies atomic.Pointer[proxySettings]

var errCGIProxy = errors.New("refusing to use the http_proxy variable in a CGI environment, where a request header can set it")

// Proxy variables of the environment, false when it sets none of http_proxy, https_proxy and all_proxy in either case
func envProxyKey() (proxyKey, bool) {
	env := httpproxy.FromEnvironment()
	all := getenvAny("all_proxy", "ALL_PROXY")
	key := proxyKey{noProxy: env.NoProxy}
	if env.HTTPProxy == "" && env.HTTPSProxy == "" && all == "" {
		return key, false
	}
	key.http, key.https = cmp.Or(env.HTTPProxy, all), cmp.Or(env.HTTPSProxy, all)
	key.cgiHTTP = env.CGI && env.HTTPProxy != ""
	return key, true
}

// Settings in force now, the environment first and the operating system when it sets no proxy
func currentProxies() *proxySettings {
	key, ok := envProxyKey()
	if !ok {
		sys := systemSnapshot()
		key.http, key.https, key.system = sys.proxies["http"], sys.proxies["https"], sys
	}
	return settingsFor(key)
}

// Parsed settings of the key, reused while the configuration stays the same
func settingsFor(key proxyKey) *proxySettings {
	if s := lastProxies.Load(); s != nil && s.key == key {
		return s
	}
	source := "the environment"
	if key.system != nil {
		source = "the system proxy settings"
	}
	s := &proxySettings{key: key, noProxy: noProxyMatcher(key.noProxy)}
	s.http, s.httpErr = parseProxyURL(key.http, source)
	s.https, s.httpsErr = parseProxyURL(key.https, source)
	lastProxies.Store(s)
	return s
}

func (s *proxySettings) proxyFor(u *url.URL) (*url.URL, error) {
	var scheme string
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		scheme = "https"
	case "http", "ws":
		scheme = "http"
	default:
		return nil, nil
	}
	proxy, err := s.http, s.httpErr
	if scheme == "https" {
		proxy, err = s.https, s.httpsErr
	}
	if proxy == nil && err == nil {
		return nil, nil
	}
	target := u
	if u.Scheme != scheme {
		c := *u
		c.Scheme = scheme
		target = &c
	}
	if s.bypasses(target) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if scheme == "http" && s.key.cgiHTTP {
		return nil, errCGIProxy
	}
	out := *proxy
	return &out, nil
}

func (s *proxySettings) bypasses(u *url.URL) bool {
	return s.noProxy(u) || s.key.system != nil && s.key.system.bypasses(u.Hostname())
}

func (s *proxySettings) table() map[string]string {
	out := make(map[string]string, 2)
	if s.http != nil && !s.key.cgiHTTP {
		out["http"] = s.http.String()
	}
	if s.https != nil {
		out["https"] = s.https.String()
	}
	return out
}

// Proxy URL of a setting, http assumed for a bare host and port, an error naming the source for a value no transport can use
func parseProxyURL(raw, source string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		if withScheme, err2 := url.Parse("http://" + raw); err2 == nil && withScheme.Host != "" {
			u, err = withScheme, nil
		}
	}
	// The setting goes into the error text with its password masked, and a *url.Error quoting it whole is unwrapped to its cause
	shown := redactPassword(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("proxy %q from %s is not a URL: %w", shown, source, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("proxy %q from %s uses the unsupported scheme %q", shown, source, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q from %s names no host", shown, source)
	}
	return u, nil
}

// Proxy the NO_PROXY matcher is built over, only whether it comes back matters
const matcherProxy = "http://no-proxy.invalid"

type noProxyCache struct {
	list  string
	match func(*url.URL) bool
}

var lastNoProxy atomic.Pointer[noProxyCache]

// Whether the NO_PROXY list exempts an http or https URL, the matching of golang.org/x/net/http/httpproxy, a loopback host always exempt
func noProxyMatcher(list string) func(*url.URL) bool {
	if c := lastNoProxy.Load(); c != nil && c.list == list {
		return c.match
	}
	f := (&httpproxy.Config{HTTPProxy: matcherProxy, HTTPSProxy: matcherProxy, NoProxy: list}).ProxyFunc()
	match := func(u *url.URL) bool {
		if loopbackHost(u.Hostname()) {
			return true
		}
		p, err := f(u)
		return err == nil && p == nil
	}
	lastNoProxy.Store(&noProxyCache{list: list, match: match})
	return match
}

// Whether a host is this machine: localhost or a name under it, in any case and with or without a trailing dot, or a loopback address, zone or IPv4 mapping aside; httpproxy alone exempts only the exact lowercase localhost
func loopbackHost(host string) bool {
	host = strings.TrimSuffix(host, ".")
	const suffix = ".localhost"
	if strings.EqualFold(host, "localhost") || len(host) > len(suffix) && strings.EqualFold(host[len(host)-len(suffix):], suffix) {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// HTTPS URL of a host given with or without a port, a bare IPv6 address bracketed
func hostURL(host string) *url.URL {
	host = strings.TrimSpace(host)
	if strings.Count(host, ":") > 1 && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return &url.URL{Scheme: "https", Host: host}
}

// First non empty variable of the names
func getenvAny(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

// Operating system proxy settings as read at one moment
type systemSettings struct {
	// Proxy URL per scheme, keys http and https
	proxies map[string]string
	// Host names without a dot bypass the proxy
	bypassSimple bool
	// Lowercased host patterns, addresses and address ranges that bypass the proxy
	exceptions []string
	loaded     time.Time
}

// How long one reading of the operating system settings is used before they are read again
const systemRefresh = 30 * time.Second

var (
	systemMu   sync.Mutex
	systemLast atomic.Pointer[systemSettings]
)

// Operating system settings, read again once the last reading is older than systemRefresh
func systemSnapshot() *systemSettings {
	if s := systemLast.Load(); s != nil && time.Since(s.loaded) < systemRefresh {
		return s
	}
	systemMu.Lock()
	defer systemMu.Unlock()
	if s := systemLast.Load(); s != nil && time.Since(s.loaded) < systemRefresh {
		return s
	}
	s := loadSystemSettings()
	if s.proxies == nil {
		s.proxies = map[string]string{}
	}
	exceptions := s.exceptions[:0]
	for _, e := range s.exceptions {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			exceptions = append(exceptions, e)
		}
	}
	s.exceptions = exceptions
	s.loaded = time.Now()
	systemLast.Store(s)
	return s
}

// Whether the settings exempt a host name or address: a name without a dot when simple names bypass, an address inside a listed address or range, or a name or address matching a listed pattern
func (s *systemSettings) bypasses(host string) bool {
	if s == nil {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	addr, err := netip.ParseAddr(host)
	isIP := err == nil
	if isIP {
		// A zoned address never falls inside a netip.Prefix, so the zone goes before the range tests
		addr = addr.Unmap().WithZone("")
	} else if s.bypassSimple && !strings.Contains(host, ".") {
		return true
	}
	for _, pattern := range s.exceptions {
		if isIP {
			if prefix, ok := addressRange(pattern); ok {
				if prefix.Contains(addr) {
					return true
				}
				continue
			}
		}
		if matched, err := path.Match(globPattern(pattern), host); err == nil && matched {
			return true
		}
	}
	return false
}

// Address range a bypass entry names: a CIDR range, a single address with or without brackets, or a dotted IPv4 prefix such as 169.254/16 or 10 whose length defaults to its parts
func addressRange(pattern string) (netip.Prefix, bool) {
	if inner, ok := strings.CutPrefix(pattern, "["); ok {
		if addr, rest, found := strings.Cut(inner, "]"); found {
			pattern = addr + rest
		}
	}
	if p, err := netip.ParsePrefix(pattern); err == nil {
		return p.Masked(), true
	}
	if a, err := netip.ParseAddr(pattern); err == nil {
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	addrPart, bitsPart, hasBits := strings.Cut(pattern, "/")
	parts := strings.Split(addrPart, ".")
	if len(parts) > 4 {
		return netip.Prefix{}, false
	}
	var b [4]byte
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 10, 8)
		if err != nil {
			return netip.Prefix{}, false
		}
		b[i] = byte(n)
	}
	bits := 8 * len(parts)
	if hasBits {
		n, err := strconv.Atoi(bitsPart)
		if err != nil || n < 0 || n > 32 {
			return netip.Prefix{}, false
		}
		bits = n
	}
	return netip.PrefixFrom(netip.AddrFrom4(b), bits).Masked(), true
}

// Glob of a bypass entry for path.Match, a scheme and a port it carries dropped and a [! class written as [^
func globPattern(pattern string) string {
	if _, rest, ok := strings.Cut(pattern, "://"); ok {
		pattern = rest
	}
	if i := strings.LastIndexByte(pattern, ':'); i >= 0 && strings.Count(pattern, ":") == 1 {
		if _, err := strconv.ParseUint(pattern[i+1:], 10, 16); err == nil {
			pattern = pattern[:i]
		}
	}
	return strings.ReplaceAll(pattern, "[!", "[^")
}
