package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Logged in place of a URL no part of which can be told apart from a credential
const unsafeURLPlaceholder = "<unparseable URL redacted>"

var (
	// Host worth naming, a DNS name or IPv4 address, or an IPv6 literal with a zone id
	plausibleHostRE = regexp.MustCompile(`^(?:[\p{L}\p{N}_.-]+|[0-9a-f:.]+(%[\p{L}\p{N}_.-]+)?)$`)
	// URL that is nothing but scheme://host[:port], the only shape whose rendering shows all of it
	bareAuthorityRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*)://(\[[^\]]*\]|[^:/@?#\[\]]*)(?::([0-9]{1,5}))?/*$`)
)

// Proxy variables a transport reads from the environment, by lower case name
var envProxyKeys = map[string]bool{"https_proxy": true, "http_proxy": true, "all_proxy": true, "wss_proxy": true, "ws_proxy": true, "socks_proxy": true, "grpc_proxy": true}

// Host and port of a network location the way a URL parser reads them, false for a port that is not a number in range
func netlocHostPort(netloc string) (host, port string, ok bool) {
	hostinfo := netloc[strings.LastIndex(netloc, "@")+1:]
	if _, bracketed, open := strings.Cut(hostinfo, "["); open {
		var rest string
		host, rest, _ = strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(rest, ":")
	} else {
		host, port, _ = strings.Cut(hostinfo, ":")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || strings.Trim(port, "0123456789") != "" || n > 65535 {
			return "", "", false
		}
		port = strconv.Itoa(n)
	}
	return strings.ToLower(host), port, true
}

// Last path segment worth keeping in a log line, a media file name and never a bare token
var logFileNameRE = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,40}\.[A-Za-z0-9]{1,5}$`)

// Text cut to a length for a log line, an ellipsis standing for what was cut
func fitForLog(safe string, maxLen int) string {
	runes := []rune(safe)
	switch {
	case len(runes) <= maxLen:
		return safe
	case maxLen <= 3:
		return strings.Repeat(".", maxLen)
	}
	return string(runes[:maxLen-3]) + "..."
}

// URL for a log line: scheme://host[:port] and the file name of a media path, never the query, fragment, user info or any other path segment; a
// local file URL without an authority is kept up to its query; text with no parsed authority is replaced by a fixed placeholder, since no part of
// it can be told apart from a credential
func SafeURLForLog(raw string, maxLen int) string {
	if maxLen <= 0 || raw == "" {
		return ""
	}
	parsed, err := pyurl.Split(raw)
	if err != nil {
		return placeholderForLog(maxLen)
	}
	if strings.EqualFold(parsed.Scheme, "file") && parsed.Netloc == "" && !strings.Contains(raw, "@") {
		local, _, _ := strings.Cut(raw, "?")
		local, _, _ = strings.Cut(local, "#")
		return fitForLog(local, maxLen)
	}
	authority := proxyAuthority(raw)
	if authority == unsafeURLPlaceholder || strings.EqualFold(parsed.Scheme, "file") {
		return placeholderForLog(maxLen)
	}
	tail := ""
	if path := parsed.Path; path != "" && path != "/" {
		tail = "/..."
		if base := path[strings.LastIndex(path, "/")+1:]; logFileNameRE.MatchString(base) {
			tail = "/.../" + base
		}
	}
	return fitForLog(authority+tail, maxLen)
}

// Placeholder for a URL that cannot be shown, cut to the length without an ellipsis
func placeholderForLog(maxLen int) string {
	runes := []rune(unsafeURLPlaceholder)
	return string(runes[:min(maxLen, len(runes))])
}

// Longest rendering of a proxy URL a log line shows before it is cut
const proxyURLLogLimit = 80

// scheme://host[:port] of a proxy URL for a log line, cut at the log limit, the placeholder when the URL has no clean authority
func SafeProxyURLForLog(raw string) string {
	safe := []rune(proxyAuthority(raw))
	if len(safe) <= proxyURLLogLimit {
		return string(safe)
	}
	return string(safe[:proxyURLLogLimit-3]) + "..."
}

// scheme://host[:port] of a proxy URL in full, the placeholder when the URL has no clean authority: a proxy URL's path means nothing to routing, so none
// of it is shown, and neither is the user info
func proxyAuthority(raw string) string {
	parsed, err := pyurl.Split(raw)
	if raw == "" {
		return ""
	}
	if err != nil || parsed.Scheme == "" || parsed.Netloc == "" || strings.EqualFold(parsed.Scheme, "file") {
		return unsafeURLPlaceholder
	}
	host, port, ok := netlocHostPort(parsed.Netloc)
	if !ok || host == "" || strings.Count(raw, "@") != strings.Count(parsed.Netloc, "@") || !plausibleHostRE.MatchString(host) {
		return unsafeURLPlaceholder
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return strings.ToLower(parsed.Scheme) + "://" + host
}

// Scheme a proxy library reads from a value, a bare host:port taken as http
func proxyScheme(value string) string {
	if scheme, _, found := strings.Cut(value, "://"); found {
		return strings.ToLower(scheme)
	}
	return "http"
}

// Whether the rendering of a proxy URL prints all of it, so that an error text naming it can repeat nothing left out
func proxyShownInFull(proxy string) bool {
	if !strings.Contains(proxy, "://") {
		proxy = "http://" + proxy
	}
	m := bareAuthorityRE.FindStringSubmatch(proxy)
	if m == nil {
		return false
	}
	canonical := strings.ToLower(m[1]) + "://" + strings.ToLower(m[2])
	if m[3] != "" {
		port, _ := strconv.Atoi(m[3])
		canonical += ":" + strconv.Itoa(port)
	}
	return proxyAuthority(proxy) == canonical
}

// "<error type> via <scheme://host:port>" for an error raised through a proxy URL that holds anything its rendering leaves out, user info, a path, a
// query or a malformed authority; empty when the proxy holds nothing of the kind and the caller keeps the full error text. A transport may repeat the
// proxy URL in its error text quoted, split or reformatted, so no part of that text is shown
func DescribeProxyError(err error, proxyURL string) string {
	if err == nil || proxyURL == "" || proxyShownInFull(proxyURL) {
		return ""
	}
	return proxyErrorKind(err) + " via " + proxyAuthority(proxyURL)
}

// Kind of an error named in a redacted line, the exception class it stands for when it declares one and its Go type otherwise
func proxyErrorKind(err error) string {
	var named pyrepr.PyTypeNamed
	if errors.As(err, &named) && named.PyTypeName() != "" {
		return named.PyTypeName()
	}
	if kind := pyErrorKind(err); kind != "" {
		return kind
	}
	if pyrepr.ExceptionMessage(err) != err.Error() {
		return pyrepr.ExceptionName(err)
	}
	return fmt.Sprintf("%T", err)
}

// Text of a failed request for a log line or a send result: the redacted form when the request went through a proxy whose URL holds a secret the raw
// text could repeat, the raw text otherwise
func ProxySafeErrorText(err error, proxyURL string) string {
	if shown := DescribeProxyError(err, proxyURL); shown != "" {
		return shown
	}
	return err.Error()
}

// Proxy value of the environment to hand DescribeProxyError for an error raised by a transport that reads the proxy environment itself: a value that
// cannot be shown in full before any that can, none when the environment sets no proxy or NO_PROXY keeps every target off the proxy
func EnvProxyForRedaction(targets ...string) string {
	var values []string
	var noProxies []string
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		switch key := strings.ToLower(name); {
		case envProxyKeys[key] && strings.TrimSpace(value) != "":
			values = append(values, strings.TrimSpace(value))
		case key == "no_proxy":
			noProxies = append(noProxies, value)
		}
	}
	var named []string
	for _, target := range targets {
		if target != "" {
			named = append(named, target)
		}
	}
	if len(named) > 0 && noProxyBypassesAll(values, noProxies, named) {
		return ""
	}
	for _, value := range values {
		if !proxyShownInFull(value) {
			return value
		}
	}
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

// Proxy value of the environment to hand DescribeProxyError for an error raised by a client that reads the proxy environment only while the gateway
// trusts it, none when gateway.trust_env is off
func TrustedEnvProxyForRedaction(ctx context.Context, targets ...string) string {
	if !GatewayTrustEnv(ctx) {
		return ""
	}
	return EnvProxyForRedaction(targets...)
}

// Whether no transport could have used a proxy for any of the targets: every proxy value parses, a value holding a secret is http or https, and every
// NO_PROXY variable is the wildcard or names each target through an entry every transport reads alike
func noProxyBypassesAll(values, noProxies, targets []string) bool {
	for _, value := range values {
		probe := value
		if !strings.Contains(probe, "://") {
			probe = "http://" + probe
		}
		if proxyAuthority(probe) == unsafeURLPlaceholder {
			return false
		}
		if scheme := proxyScheme(value); !proxyShownInFull(value) && scheme != "http" && scheme != "https" {
			return false
		}
	}
	if os.Getenv("no_proxy") == "" && os.Getenv("NO_PROXY") == "" {
		return false
	}
	for _, target := range targets {
		parsed, err := pyurl.Split(target)
		if err != nil {
			return false
		}
		host, _, ok := netlocHostPort(parsed.Netloc)
		if !ok {
			return false
		}
		netloc := parsed.Netloc[strings.LastIndex(parsed.Netloc, "@")+1:]
		for _, noProxy := range noProxies {
			if !noProxyBypasses(noProxy, host, netloc) {
				return false
			}
		}
	}
	return len(noProxies) > 0
}

// Whether a NO_PROXY value keeps a host off the proxy for every transport, which read the entries with different rules
func noProxyBypasses(noProxy, host, netloc string) bool {
	if noProxy == "*" {
		return true
	}
	ascii := func(s string) bool { return strings.IndexFunc(s, func(r rune) bool { return r > 127 }) < 0 }
	if host == "" || strings.Contains(noProxy, "*") || !ascii(host) || !ascii(netloc) || !ascii(noProxy) || strings.Contains(strings.ToLower(host), "xn--") {
		return false
	}
	proxies := map[string]string{"no": noProxy}
	if !pyurl.ProxyBypassEnvironment(host, proxies) || !pyurl.ProxyBypassEnvironment(netloc, proxies) {
		return false
	}
	for _, entry := range strings.Split(noProxy, ",") {
		if entry == host || (strings.HasPrefix(entry, ".") && strings.HasSuffix(host, entry) && host != entry[1:]) {
			return true
		}
	}
	return false
}
