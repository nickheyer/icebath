package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// Where connections find their proxy, an explicit URL first, then environment variables, then the OS settings
type ProxyOptions struct {
	URL       string
	IgnoreEnv bool
	System    bool
	AnyScheme bool
}

// Proxy variables in the precedence that picks one proxy for every target scheme
var anySchemeKeys = []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"}

// Reasons a proxy setting is rejected, none of them echoing any part of the setting
var (
	errProxyURL    = errors.New("not a valid URL")
	errProxyScheme = errors.New("scheme is not http, https, socks4, socks4a, socks5 or socks5h")
	errProxyHost   = errors.New("no host")
	errProxyPort   = errors.New("port is not a number from 1 to 65535")
)

// Proxy setting that cannot be turned into a usable proxy URL, its credentials masked
type ProxyConfigError struct {
	Source string
	Value  string
	Err    error
}

func (e *ProxyConfigError) Error() string {
	if e.Source != "" {
		return fmt.Sprintf("Malformed proxy environment variable %s='%s'. Fix or unset your proxy settings and try again.", e.Source, e.Value)
	}
	return fmt.Sprintf("Malformed proxy URL '%s': %v", e.Value, e.Err)
}

func (e *ProxyConfigError) Unwrap() error { return e.Err }

// Rejects a malformed value among the six proxy variables of the process environment
func ValidateProxyEnv() error {
	for _, key := range anySchemeKeys {
		if value := envValue(key); value != "" {
			if _, err := parseProxy(value, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// Proxy URL of a setting, socks rewritten to socks5 and a bare host and port taken as an HTTP proxy
func parseProxy(raw, source string) (*url.URL, error) {
	value := normalizeProxy(raw)
	fail := func(reason error) (*url.URL, error) {
		return nil, &ProxyConfigError{Source: source, Value: maskUserinfo(value), Err: reason}
	}
	full := value
	if !strings.Contains(full, "://") {
		full = "http://" + full
	}
	u, err := url.Parse(full)
	if err != nil {
		return fail(errProxyURL)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks4", "socks4a", "socks5", "socks5h":
	default:
		return fail(errProxyScheme)
	}
	if u.Hostname() == "" {
		return fail(errProxyHost)
	}
	if text := u.Port(); text != "" {
		if n, ok := portNumber(text); !ok || n == 0 {
			return fail(errProxyPort)
		}
	}
	return &url.URL{Scheme: scheme, User: u.User, Host: u.Host}, nil
}

// Setting with whitespace trimmed and the socks scheme exported by WSL and Clash rewritten to socks5
func normalizeProxy(raw string) string {
	value := strings.TrimSpace(raw)
	if len(value) >= len("socks://") && strings.EqualFold(value[:len("socks://")], "socks://") {
		value = "socks5://" + value[len("socks://"):]
	}
	return value
}

// Setting with its password, or a userinfo holding only a token, replaced by asterisks up to the last at sign
func maskUserinfo(value string) string {
	start := 0
	if i := strings.Index(value, "://"); i >= 0 {
		start = i + 3
	}
	at := strings.LastIndex(value[start:], "@")
	if at < 0 {
		return value
	}
	name, _, hasPassword := strings.Cut(value[start:start+at], ":")
	if !hasPassword {
		return value[:start] + "***" + value[start+at:]
	}
	return value[:start] + name + ":***" + value[start+at:]
}

// Scheme and host of a proxy without its credentials, for error text
func proxyLabel(proxy *url.URL) string { return proxy.Scheme + "://" + proxy.Host }

// Dial address of a proxy with the default port of its scheme filled in
func proxyAddr(proxy *url.URL) string {
	if proxy.Port() != "" {
		return proxy.Host
	}
	port := "1080"
	switch proxy.Scheme {
	case "http":
		port = "80"
	case "https":
		port = "443"
	}
	return net.JoinHostPort(proxy.Hostname(), port)
}

// Variable name and value of the environment proxy for a target scheme, empty when none is set
func envProxy(scheme string, anyScheme bool) (string, string) {
	if anyScheme {
		for _, key := range anySchemeKeys {
			if value := envValue(key); value != "" {
				return key, value
			}
		}
		return "", ""
	}
	_, cgi := os.LookupEnv("REQUEST_METHOD")
	for _, name := range []string{scheme, "all"} {
		lower := name + "_proxy"
		if value, ok := os.LookupEnv(lower); ok {
			if strings.TrimSpace(value) != "" {
				return lower, value
			}
			continue
		}
		if name == "http" && cgi {
			continue
		}
		upper := strings.ToUpper(lower)
		if value := envValue(upper); value != "" {
			return upper, value
		}
	}
	return "", ""
}

// Ordered proxies to try for a target, a nil entry meaning a direct connection
func (t *Transport) routesFor(ctx context.Context, target *url.URL) ([]*url.URL, error) {
	host, port := bypassTarget(target)
	if IsLoopbackHost(host) || envBypass().match(host, port) {
		return []*url.URL{nil}, nil
	}
	if t.opts.Proxy.URL != "" {
		proxy, err := parseProxy(t.opts.Proxy.URL, "")
		if err != nil {
			return nil, err
		}
		return []*url.URL{proxy}, nil
	}
	if !t.opts.Proxy.IgnoreEnv {
		scheme := "http"
		if s := strings.ToLower(target.Scheme); s == "https" || s == "wss" {
			scheme = "https"
		}
		if key, value := envProxy(scheme, t.opts.Proxy.AnyScheme); key != "" {
			proxy, err := parseProxy(value, key)
			if err != nil {
				return nil, err
			}
			return []*url.URL{proxy}, nil
		}
	}
	if t.opts.Proxy.System {
		routes, err := t.system.lookup(ctx, target, t.opts.Proxy.AnyScheme)
		if err != nil {
			return nil, fmt.Errorf("reading the operating system proxy settings: %w", err)
		}
		if len(routes) > 0 {
			return routes, nil
		}
	}
	return []*url.URL{nil}, nil
}

// Proxy the transport would use first for a target, nil for a direct connection
func (t *Transport) ProxyFor(ctx context.Context, target *url.URL) (*url.URL, error) {
	routes, err := t.routesFor(ctx, target)
	if err != nil {
		return nil, err
	}
	return routes[0], nil
}

// Host and port a target URL dials, an international host in its ASCII form and the default port filled in
func dialTarget(u *url.URL) string {
	_, port := targetHostPort(u)
	if port < 0 {
		return u.Host
	}
	host := strings.ToLower(u.Hostname())
	for _, r := range host {
		if r >= utf8.RuneSelf {
			if ascii, err := idna.Lookup.ToASCII(host); err == nil {
				host = ascii
			}
			break
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
