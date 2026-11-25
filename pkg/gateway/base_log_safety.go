package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
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
	parsed, err := urlutil.Split(raw)
	if err != nil {
		return placeholderForLog(maxLen)
	}
	if strings.EqualFold(parsed.Scheme, "file") && parsed.Authority == "" && !strings.Contains(raw, "@") {
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
	if raw == "" {
		return ""
	}
	parsed, err := urlutil.Split(raw)
	if err != nil || parsed.Scheme == "" || parsed.Authority == "" || strings.EqualFold(parsed.Scheme, "file") {
		return unsafeURLPlaceholder
	}
	host := strings.ToLower(parsed.Hostname())
	port, hasPort, err := parsed.Port()
	if err != nil || host == "" || strings.Count(raw, "@") != strings.Count(parsed.Authority, "@") || !plausibleHostRE.MatchString(host) {
		return unsafeURLPlaceholder
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if hasPort {
		host += ":" + strconv.Itoa(port)
	}
	return parsed.Scheme + "://" + host
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

// "<failure> via <scheme://host:port>" for an error raised through a proxy URL that holds anything its rendering leaves out, user info, a path, a
// query or a malformed authority; empty when the proxy holds nothing of the kind and the caller keeps the full error text. A transport may repeat the
// proxy URL in its error text quoted, split or reformatted, so no part of that text is shown
func DescribeProxyError(err error, proxyURL string) string {
	if err == nil || proxyURL == "" || proxyShownInFull(proxyURL) {
		return ""
	}
	return proxyFailureReason(err) + " via " + proxyAuthority(proxyURL)
}

// Short reason of a request failure read from the errors it wraps, never from its text: a timeout, a refused, reset or closed connection, a failed
// name lookup, a TLS or certificate failure, else "request failed"
func proxyFailureReason(err error) string {
	var netErr net.Error
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &dnsErr):
		return "name lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	case errors.As(err, &certErr):
		return "certificate verification failed"
	case errors.As(err, &recordErr) || errors.As(err, &alertErr):
		return "TLS handshake failed"
	}
	return "request failed"
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
		parsed, err := urlutil.Split(target)
		if err != nil {
			return false
		}
		if _, _, err := parsed.Port(); err != nil {
			return false
		}
		host := strings.ToLower(parsed.Hostname())
		netloc := parsed.Authority[strings.LastIndex(parsed.Authority, "@")+1:]
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
	if !httputil.NoProxyBypass(noProxy, host) || !httputil.NoProxyBypass(noProxy, netloc) {
		return false
	}
	for _, entry := range strings.Split(noProxy, ",") {
		if entry == host || (strings.HasPrefix(entry, ".") && strings.HasSuffix(host, entry) && host != entry[1:]) {
			return true
		}
	}
	return false
}
