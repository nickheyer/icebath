package provider

import (
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ProxyEnvKeys are the proxy variables read in order, both casings
var ProxyEnvKeys = []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"}

// FirstProxyEnvValue is the first non empty proxy variable value, any case
func FirstProxyEnvValue() string {
	for _, k := range ProxyEnvKeys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func isDigitString(s string) bool {
	return isDigits(s)
}

// SplitHostPort is the lowercased host and optional port of a URL, a bracketed v6, a host with port or a bare host
func SplitHostPort(value string) (string, int, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", 0, false
	}
	var host string
	port, hasPort := 0, false
	switch {
	case strings.Contains(raw, "://") || strings.HasPrefix(raw, "//"):
		parsed, err := url.Parse(raw)
		if err != nil {
			host, port, hasPort = lenientAuthority(raw)
		} else {
			host = parsed.Hostname()
			if p := parsed.Port(); p != "" {
				if n, err := strconv.Atoi(p); err == nil && n >= 0 && n <= 65535 {
					port, hasPort = n, true
				}
			}
		}
	case strings.HasPrefix(raw, "[") && strings.Contains(raw, "]"):
		inner, rest, _ := strings.Cut(raw[1:], "]")
		host = inner
		if strings.HasPrefix(rest, ":") && isDigitString(rest[1:]) {
			port, _ = strconv.Atoi(rest[1:])
			hasPort = true
		}
	case strings.Count(raw, ":") == 1 && isDigitString(raw[strings.LastIndex(raw, ":")+1:]):
		idx := strings.LastIndex(raw, ":")
		host = raw[:idx]
		port, _ = strconv.Atoi(raw[idx+1:])
		hasPort = true
	default:
		host = strings.Trim(raw, "[]")
	}
	return strings.TrimRight(strings.ToLower(host), "."), port, hasPort
}

// Host and port of a URL whose port the parser rejected, keeping the host and dropping the port
func lenientAuthority(raw string) (string, int, bool) {
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	} else {
		rest = strings.TrimPrefix(rest, "//")
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if strings.HasPrefix(rest, "[") {
		inner, after, _ := strings.Cut(rest[1:], "]")
		if strings.HasPrefix(after, ":") && isDigitString(after[1:]) {
			if n, err := strconv.Atoi(after[1:]); err == nil && n <= 65535 {
				return inner, n, true
			}
		}
		return inner, 0, false
	}
	host, portText, found := strings.Cut(rest, ":")
	if found && isDigitString(portText) {
		if n, err := strconv.Atoi(portText); err == nil && n <= 65535 {
			return host, n, true
		}
	}
	return host, 0, false
}

var noProxySplitRe = regexp.MustCompile(`[\s,]+`)

// NoProxyEntries splits a NO_PROXY value, reading both environment casings when nil is passed
func NoProxyEntries(value *string) []string {
	raw := ""
	if value == nil {
		raw = os.Getenv("NO_PROXY") + "," + os.Getenv("no_proxy")
	} else {
		raw = *value
	}
	var out []string
	for _, part := range noProxySplitRe.Split(strings.TrimSpace(raw), -1) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// LoopbackHosts must never be dialed through a proxy
var LoopbackHosts = []string{"127.0.0.1", "localhost", "::1"}

func parseIP(value string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}

// IsProxyLoopbackHost is true for localhost or any loopback IP literal including the v4 mapped form
func IsProxyLoopbackHost(host string) bool {
	h := strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if h == "localhost" {
		return true
	}
	addr, ok := parseIP(h)
	if !ok {
		return false
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	return addr.IsLoopback()
}

// LoopbackBypassesProxy reports whether an in process dial of the URL must skip proxy detection
func LoopbackBypassesProxy(rawURL string) bool {
	host, _, _ := SplitHostPort(rawURL)
	return IsProxyLoopbackHost(host)
}

// AddLoopbackNoProxy appends the loopback hosts to both NO_PROXY casings in env keeping operator entries
func AddLoopbackNoProxy(env map[string]string) map[string]string {
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		v := env[key]
		for _, e := range NoProxyEntries(&v) {
			if strings.Contains(e, "*") {
				return env
			}
		}
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		v := env[key]
		entries := NoProxyEntries(&v)
		var missing []string
		for _, host := range LoopbackHosts {
			if !containsString(entries, host) {
				missing = append(missing, host)
			}
		}
		if len(missing) > 0 {
			env[key] = strings.Join(append(entries, missing...), ",")
		}
	}
	return env
}

func parseNetwork(value string) (netip.Prefix, bool) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Masked(), true
	}
	if addr, ok := parseIP(value); ok {
		return netip.PrefixFrom(addr, addr.BitLen()), true
	}
	return netip.Prefix{}, false
}

// NoProxyEntryMatches reports whether one NO_PROXY entry covers the host and port
func NoProxyEntryMatches(entry, host string, port int, hasPort bool) bool {
	token := strings.ToLower(strings.TrimSpace(entry))
	if token == "" {
		return false
	}
	if token == "*" {
		return true
	}
	tokenHost, tokenPort, tokenHasPort := SplitHostPort(token)
	if tokenHost == "" || (tokenHasPort && (!hasPort || tokenPort != port)) {
		return false
	}
	hostIP, hostIsIP := parseIP(host)
	if network, ok := parseNetwork(tokenHost); ok {
		if !hostIsIP {
			return false
		}
		if hostIP.Is4In6() && network.Addr().Is4() {
			hostIP = hostIP.Unmap()
		}
		return network.Contains(hostIP)
	}
	suffix := strings.TrimPrefix(strings.TrimPrefix(tokenHost, "*"), ".")
	return host == suffix || strings.HasSuffix(host, "."+suffix)
}

// ShouldBypassProxy is true when NO_PROXY matches at least one target host URL or host port
func ShouldBypassProxy(targets []string, noProxyValue *string) bool {
	entries := NoProxyEntries(noProxyValue)
	if len(entries) == 0 || len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		host, port, hasPort := SplitHostPort(target)
		if host == "" {
			continue
		}
		for _, entry := range entries {
			if NoProxyEntryMatches(entry, host, port, hasPort) {
				return true
			}
		}
	}
	return false
}

// HostOf is the lowercased host of a URL or host port string
func HostOf(value string) string {
	host, _, _ := SplitHostPort(value)
	if host == "" {
		if h, _, err := net.SplitHostPort(value); err == nil {
			return strings.ToLower(h)
		}
	}
	return host
}
