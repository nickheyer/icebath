package tools

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/idna"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Proxy env vars that delegate DNS to the proxy when set
var proxyEnvVars = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"}

func proxyConfigured() bool {
	for _, v := range proxyEnvVars {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

func isHTTPScheme(scheme string) bool {
	s := strings.ToLower(scheme)
	return s == "http" || s == "https"
}

// urlParts mirrors urlsplit so authority checks see the same bytes Python did
type urlParts struct {
	scheme, netloc, path, query, fragment string
	hasNetloc                             bool
}

var schemeCharsRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*$`)

// Splits a URL into scheme, authority, path, query and fragment without decoding anything
func splitURL(raw string) urlParts {
	var p urlParts
	rest := raw
	if i := strings.Index(rest, ":"); i > 0 && schemeCharsRe.MatchString(rest[:i]) {
		p.scheme = rest[:i]
		rest = rest[i+1:]
	}
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		end := len(rest)
		for i, c := range rest {
			if c == '/' || c == '?' || c == '#' {
				end = i
				break
			}
		}
		p.netloc, rest = rest[:end], rest[end:]
		p.hasNetloc = true
	}
	if i := strings.Index(rest, "#"); i >= 0 {
		p.fragment, rest = rest[i+1:], rest[:i]
	}
	if i := strings.Index(rest, "?"); i >= 0 {
		p.query, rest = rest[i+1:], rest[:i]
	}
	p.path = rest
	return p
}

// Host portion of an authority, bracket literal kept bare and lowercased
func netlocHost(netloc string) string {
	host := netloc
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if strings.HasPrefix(host, "[") {
		if j := strings.Index(host, "]"); j >= 0 {
			return strings.ToLower(host[1:j])
		}
		return strings.ToLower(host[1:])
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return strings.ToLower(host)
}

// Port text of an authority, empty when absent, false when not a valid port number
func netlocPort(netloc string) (string, bool) {
	host := netloc
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if strings.HasPrefix(host, "[") {
		j := strings.Index(host, "]")
		if j < 0 {
			return "", true
		}
		host = host[j+1:]
		if host == "" {
			return "", true
		}
		if !strings.HasPrefix(host, ":") {
			return "", false
		}
		host = host[1:]
	} else {
		i := strings.LastIndex(host, ":")
		if i < 0 {
			return "", true
		}
		host = host[i+1:]
	}
	if host == "" {
		return "", true
	}
	n, err := strconv.Atoi(host)
	if err != nil || n < 0 || n > 65535 {
		return host, false
	}
	return host, true
}

// Percent encodes every byte outside the unreserved set and the caller's safe set
func quoteURLPart(s, safe string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '.', c == '-', c == '~':
			b.WriteByte(c)
		case strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

var schemeSpaceRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.\-]*://)\s+`)

// ASCII safe HTTP URL for URL tools, IDNA host and percent encoded non ASCII path text
func normalizeURLForRequest(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	raw = schemeSpaceRe.ReplaceAllString(raw, "$1")
	parts := splitURL(raw)
	if !isHTTPScheme(parts.scheme) {
		return raw
	}
	netloc := parts.netloc
	if hostname := netlocHost(netloc); hostname != "" {
		original := hostname
		if idx := strings.Index(strings.ToLower(netloc), hostname); idx >= 0 {
			original = netloc[idx : idx+len(hostname)]
		}
		if ascii, err := idna.Lookup.ToASCII(original); err == nil && ascii != original {
			netloc = strings.Replace(netloc, original, ascii, 1)
		}
	}
	safe := "/%:@!$&'()*+,;="
	out := parts.scheme + "://" + netloc + quoteURLPart(parts.path, safe)
	if parts.query != "" || strings.Contains(raw, "?") {
		out += "?" + quoteURLPart(parts.query, safe+"?")
	}
	if parts.fragment != "" || strings.Contains(raw, "#") {
		out += "#" + quoteURLPart(parts.fragment, safe+"?")
	}
	return out
}

// Url re quoted the way a request client sends it, path query and fragment percent encoded
func NormalizeURLForRequest(raw string) string { return normalizeURLForRequest(raw) }

// Credential bearing query parameter names, deliberately narrow
var sensitiveQueryParamNames = map[string]bool{
	"access_token": true, "api_key": true, "apikey": true, "auth_token": true, "authorization": true, "awsaccesskeyid": true,
	"client_secret": true, "credential": true, "credentials": true, "jwt": true, "password": true, "passwd": true, "secret": true,
	"session_id": true, "signature": true, "token": true, "x_amz_security_token": true, "x_amz_signature": true,
	"x-amz-security-token": true, "x-amz-signature": true,
}

// First credential named query parameter with a value, empty when none
func sensitiveQueryParamName(raw string) string {
	if !strings.Contains(raw, "?") {
		return ""
	}
	parts := splitURL(strings.TrimSpace(raw))
	if !isHTTPScheme(parts.scheme) || parts.query == "" {
		return ""
	}
	for _, pair := range strings.FieldsFunc(parts.query, func(r rune) bool { return r == '&' || r == ';' }) {
		key, value, _ := strings.Cut(pair, "=")
		if value == "" {
			continue
		}
		decoded, err := url.QueryUnescape(key)
		if err != nil {
			decoded = key
		}
		if sensitiveQueryParamNames[strings.ToLower(decoded)] {
			return key
		}
	}
	return ""
}

// Cloud metadata hostnames always blocked regardless of DNS or config toggle
var blockedHostnames = map[string]bool{"metadata.google.internal": true, "metadata.goog": true}

// Hostname on the cloud metadata blocklist the browser floor refuses before any resolution
func IsBlockedHostname(host string) bool { return blockedHostnames[host] }

var metadataV4 = []string{"169.254.169.254", "169.254.170.2", "169.254.169.253", "100.100.100.200"}

func mustPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

var alwaysBlockedIPs = func() map[netip.Addr]bool {
	m := map[netip.Addr]bool{}
	for _, ip := range metadataV4 {
		m[netip.MustParseAddr(ip)] = true
		m[netip.MustParseAddr("::ffff:"+ip)] = true
	}
	m[netip.MustParseAddr("fd00:ec2::254")] = true
	return m
}()

var alwaysBlockedNetworks = []netip.Prefix{mustPrefix("169.254.0.0/16"), mustPrefix("::ffff:169.254.0.0/112")}

// Exact HTTPS hostnames allowed to resolve to private or benchmark space
var trustedPrivateIPHosts = map[string]bool{"multimedia.nt.qq.com.cn": true}

const maxSSRFConnectIPs = 8

var cgnatNetwork = mustPrefix("100.64.0.0/10")

var ipv4TranslatedNetwork = mustPrefix("::ffff:0:0:0/96")

// Address classes a fake ip declaration can never excuse
var fakeIPUndeclarableNetworks = []netip.Prefix{
	mustPrefix("0.0.0.0/32"), mustPrefix("127.0.0.0/8"), mustPrefix("10.0.0.0/8"), mustPrefix("172.16.0.0/12"), mustPrefix("192.168.0.0/16"),
	mustPrefix("169.254.0.0/16"), mustPrefix("100.64.0.0/10"),
	mustPrefix("::/128"), mustPrefix("::1/128"), mustPrefix("fc00::/7"), mustPrefix("fe80::/10"),
}

// IPv4 ranges the Python ipaddress module reports as private
var pythonPrivateV4 = []netip.Prefix{
	mustPrefix("0.0.0.0/8"), mustPrefix("10.0.0.0/8"), mustPrefix("127.0.0.0/8"), mustPrefix("169.254.0.0/16"), mustPrefix("172.16.0.0/12"),
	mustPrefix("192.0.0.0/29"), mustPrefix("192.0.0.170/31"), mustPrefix("192.0.2.0/24"), mustPrefix("192.168.0.0/16"), mustPrefix("198.18.0.0/15"),
	mustPrefix("198.51.100.0/24"), mustPrefix("203.0.113.0/24"), mustPrefix("240.0.0.0/4"), mustPrefix("255.255.255.255/32"),
}

// IPv6 ranges the Python ipaddress module reports as private
var pythonPrivateV6 = []netip.Prefix{
	mustPrefix("::1/128"), mustPrefix("::/128"), mustPrefix("::ffff:0:0/96"), mustPrefix("100::/64"), mustPrefix("2001::/23"),
	mustPrefix("2001:db8::/32"), mustPrefix("2001:2::/48"), mustPrefix("fc00::/7"), mustPrefix("fe80::/10"),
}

// IPv6 ranges the Python ipaddress module reports as reserved
var pythonReservedV6 = []netip.Prefix{
	mustPrefix("::/8"), mustPrefix("100::/8"), mustPrefix("200::/7"), mustPrefix("400::/6"), mustPrefix("800::/5"), mustPrefix("1000::/4"),
	mustPrefix("4000::/3"), mustPrefix("6000::/3"), mustPrefix("8000::/3"), mustPrefix("a000::/3"), mustPrefix("c000::/3"), mustPrefix("e000::/4"),
	mustPrefix("f000::/5"), mustPrefix("f800::/6"), mustPrefix("fe00::/9"),
}

func inAny(ip netip.Addr, nets []netip.Prefix) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Resolves the private url toggle from the env override then config
func allowPrivateURLs(env *Env) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_ALLOW_PRIVATE_URLS"))) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	if env == nil || env.Config == nil {
		return false
	}
	for _, section := range []string{"security", "browser"} {
		if truthyValue(env.Config.Sub(section).Get("allow_private_urls"), false) {
			return true
		}
	}
	return false
}

// CIDR blocks the local proxy answers DNS with, undeclarable ranges dropped with a warning
func fakeIPRanges(env *Env) []netip.Prefix {
	if env == nil || env.Config == nil {
		return nil
	}
	raw := env.Config.Sub("security").Get("fake_ip_ranges")
	var entries []string
	switch v := raw.(type) {
	case string:
		entries = []string{v}
	default:
		entries = config.AsStrings(raw)
	}
	out := []netip.Prefix{}
	for _, entry := range entries {
		text := strings.TrimSpace(entry)
		prefix, err := netip.ParsePrefix(text)
		if err != nil {
			if addr, aerr := netip.ParseAddr(text); aerr == nil {
				prefix = netip.PrefixFrom(addr, addr.BitLen())
			} else {
				envLog(env).Warn("Ignoring unparseable security.fake_ip_ranges entry", "entry", entry)
				continue
			}
		}
		prefix = prefix.Masked()
		clash := false
		for _, blocked := range fakeIPUndeclarableNetworks {
			if blocked.Addr().Is4() == prefix.Addr().Is4() && prefix.Overlaps(blocked) {
				envLog(env).Warn("Ignoring security.fake_ip_ranges entry that overlaps a blocked range", "entry", entry, "blocked", blocked.String())
				clash = true
				break
			}
		}
		if !clash {
			out = append(out, prefix)
		}
	}
	return out
}

func normalizeHostname(host string) string {
	return strings.TrimRight(pystr.Lower(pystr.Strip(host)), ".")
}

const urlEdgeWhitespace = " \t\n\r\x0b\x0c"

// Dots IDNA maps to a period, used when IDNA encoding fails
var idnaDots = strings.NewReplacer("。", ".", "．", ".", "｡", ".")

// Host of the url the way a WHATWG client dials it, the urlsplit ValueError returned
func urlHostname(raw string, percentDecode bool) (string, error) {
	parts, err := pyurl.Split(strings.Trim(raw, urlEdgeWhitespace))
	if err != nil {
		return "", err
	}
	host := parts.Hostname()
	if percentDecode {
		host = pyurl.Unquote(host)
	}
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		host = ascii
	} else if ascii, err := idna.Punycode.ToASCII(host); err == nil && ascii != "" {
		host = ascii
	} else {
		host = idnaDots.Replace(host)
	}
	return normalizeHostname(host), nil
}

// Host a WHATWG client dials for the url, optionally percent decoded first, with the urlsplit ValueError
func URLHostname(raw string, percentDecode bool) (string, error) {
	return urlHostname(raw, percentDecode)
}

// Why another client could read a different host or port from the url, empty when unambiguous, the urlsplit ValueError returned
func authorityAmbiguity(raw string, floor bool) (string, error) {
	raw = strings.Trim(raw, urlEdgeWhitespace)
	parts, err := pyurl.Split(raw)
	if err != nil {
		return "", err
	}
	if !isHTTPScheme(parts.Scheme) {
		return "", nil
	}
	for _, c := range raw {
		if c < 0x20 || c == 0x7F {
			return "control character in URL", nil
		}
	}
	if parts.Netloc == "" {
		return "authority not introduced by //", nil
	}
	if strings.Contains(parts.Netloc, "\\") {
		return "backslash in authority", nil
	}
	if floor {
		return "", nil
	}
	at := strings.LastIndex(parts.Netloc, "@")
	if at >= 0 {
		return "userinfo in authority", nil
	}
	host := parts.Netloc
	if strings.Contains(host, "%") {
		return "percent-encoding in host", nil
	}
	if strings.IndexFunc(host, pystr.IsSpace) >= 0 {
		return "whitespace in host", nil
	}
	if _, _, err := parts.Port(); err != nil {
		return "invalid port", nil
	}
	return "", nil
}

// Ambiguous authority reason of an http url, the floor form keeping only host moving checks, with the urlsplit ValueError
func AuthorityAmbiguity(raw string, floor bool) (string, error) {
	return authorityAmbiguity(raw, floor)
}

// IP literal of a hostname, false for names
func parseIPLiteral(hostname string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(hostname, "["), "]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.WithZone(""), true
}

// The IPv4 address an IPv6 wrapper stands for, the input otherwise
func embeddedIPv4(ip netip.Addr) netip.Addr {
	if ip.Is4In6() {
		return ip.Unmap()
	}
	if ip.Is6() && ipv4TranslatedNetwork.Contains(ip) {
		b := ip.As16()
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	return ip
}

func isAlwaysBlockedIP(ip netip.Addr) bool {
	ip = embeddedIPv4(ip)
	return alwaysBlockedIPs[ip] || inAny(ip, alwaysBlockedNetworks)
}

// Address in the always blocked ranges such as cloud metadata and link local, independent of opt ins
func IsAlwaysBlockedIP(ip netip.Addr) bool { return isAlwaysBlockedIP(ip) }

func isDeclaredFakeIP(env *Env, ip netip.Addr) bool {
	return inAny(embeddedIPv4(ip), fakeIPRanges(env))
}

// Address inside a fake ip range the proxy configuration declares for the env
func IsDeclaredFakeIP(env *Env, ip netip.Addr) bool { return isDeclaredFakeIP(env, ip) }

// Python ipaddress classification of private, loopback, link local, reserved, multicast, unspecified and CGNAT
func isBlockedIP(ip netip.Addr) bool {
	ip = embeddedIPv4(ip)
	if ip.Is4() {
		return inAny(ip, pythonPrivateV4) || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() || cgnatNetwork.Contains(ip)
	}
	return inAny(ip, pythonPrivateV6) || inAny(ip, pythonReservedV6) || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified()
}

// Resolver swapped by tests, every address of a hostname
var lookupHostIPs = func(ctx context.Context, hostname string) ([]netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, hostname)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		out = append(out, ip.Unmap().WithZone(""))
	}
	return out, nil
}

// Reports whether the url targets the always blocked cloud metadata floor
func isAlwaysBlockedURL(env *Env, raw string) bool {
	log := envLog(env)
	reason, err := authorityAmbiguity(raw, true)
	if err != nil {
		log.Debug(fmt.Sprintf("is_always_blocked_url error for %s: %v", raw, err))
		return false
	}
	if reason != "" {
		log.Warn("Blocked request, ambiguous URL authority (always-blocked floor)", "reason", reason)
		return true
	}
	hostname, err := urlHostname(raw, true)
	if err != nil {
		log.Debug(fmt.Sprintf("is_always_blocked_url error for %s: %v", raw, err))
		return false
	}
	if hostname == "" {
		return false
	}
	if blockedHostnames[hostname] {
		log.Warn("Blocked request to internal hostname (always-blocked floor)", "host", hostname)
		return true
	}
	if ip, ok := parseIPLiteral(hostname); ok {
		if isAlwaysBlockedIP(ip) {
			log.Warn("Blocked request to cloud metadata address (always-blocked floor)", "host", hostname)
			return true
		}
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, err := lookupHostIPs(ctx, hostname)
	if err != nil {
		return false
	}
	for _, ip := range addrs {
		if isAlwaysBlockedIP(ip) {
			log.Warn("Blocked request to cloud metadata address (always-blocked floor)", "host", hostname, "ip", ip.String())
			return true
		}
	}
	return false
}

// Url the browser and media tools refuse whatever the private url opt in says, metadata and fake ip floors included
func IsAlwaysBlockedURL(env *Env, raw string) bool { return isAlwaysBlockedURL(env, raw) }

func allowsPrivateIPResolution(hostname, scheme string) bool {
	return scheme == "https" && trustedPrivateIPHosts[hostname]
}

// Why a resolved answer must be rejected, empty when it may be dialed
func resolvedIPBlockReason(env *Env, ip netip.Addr, allowPrivate bool) string {
	if isAlwaysBlockedIP(ip) {
		return "cloud metadata address"
	}
	if !allowPrivate && isBlockedIP(ip) && !isDeclaredFakeIP(env, ip) {
		return "private/internal address"
	}
	return ""
}

// Reports whether the url target is not a private or internal address, failing closed
func isSafeURL(env *Env, raw string) bool {
	log := envLog(env)
	parts, err := pyurl.Parse(raw)
	if err != nil {
		return safetyCheckError(log, raw, err)
	}
	scheme := parts.Scheme
	if !isHTTPScheme(scheme) {
		if scheme == "" {
			scheme = "<empty>"
		}
		log.Warn("Blocked request, unsupported URL scheme", "scheme", scheme)
		return false
	}
	reason, err := authorityAmbiguity(raw, false)
	if err != nil {
		return safetyCheckError(log, raw, err)
	}
	if reason != "" {
		log.Warn("Blocked request, ambiguous URL authority", "reason", reason)
		return false
	}
	hostname, err := urlHostname(raw, false)
	if err != nil {
		return safetyCheckError(log, raw, err)
	}
	if hostname == "" {
		return false
	}
	if blockedHostnames[hostname] {
		log.Warn("Blocked request to internal hostname", "host", hostname)
		return false
	}
	allowAllPrivate := allowPrivateURLs(env)
	allowPrivateIP := allowsPrivateIPResolution(hostname, scheme)
	allowPrivate := allowAllPrivate || allowPrivateIP
	var addrs []netip.Addr
	if ip, ok := parseIPLiteral(hostname); ok {
		addrs = []netip.Addr{ip}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resolved, err := lookupHostIPs(ctx, hostname)
		if err != nil {
			if proxyConfigured() {
				log.Debug("DNS resolution failed, proxy configured, allowing proxy side resolution", "host", hostname)
				return true
			}
			log.Warn("Blocked request, DNS resolution failed", "host", hostname)
			return false
		}
		addrs = resolved
	}
	for _, ip := range addrs {
		if reason := resolvedIPBlockReason(env, ip, allowPrivate); reason != "" {
			log.Warn("Blocked request", "reason", reason, "host", hostname, "ip", ip.String())
			return false
		}
	}
	if allowAllPrivate {
		log.Debug("Allowing private/internal resolution (security.allow_private_urls=true)", "host", hostname)
	} else if allowPrivateIP {
		log.Debug("Allowing trusted hostname despite private/internal resolution", "host", hostname)
	}
	return true
}

// Reports whether a URL targets a public host
func IsSafeURL(env *Env, raw string) bool { return isSafeURL(env, raw) }

// SSRFConnectionBlocked reports a connect time policy violation
type SSRFConnectionBlocked struct{ msg string }

func (e *SSRFConnectionBlocked) Error() string { return e.msg }

func ssrfBlocked(format string, a ...any) error {
	return &SSRFConnectionBlocked{msg: fmt.Sprintf(format, a...)}
}

// Resolves and validates a host at TCP connect time, every answer checked
func resolvedHTTPConnectIPs(ctx context.Context, env *Env, host string, scheme string) ([]string, error) {
	hostname := normalizeHostname(host)
	if hostname == "" {
		return nil, ssrfBlocked("Blocked request with empty hostname")
	}
	if blockedHostnames[hostname] {
		return nil, ssrfBlocked("Blocked request to internal hostname: %s", hostname)
	}
	allowPrivate := allowPrivateURLs(env) || allowsPrivateIPResolution(hostname, scheme)
	var addrs []netip.Addr
	if ip, ok := parseIPLiteral(hostname); ok {
		addrs = []netip.Addr{ip}
	} else {
		resolved, err := lookupHostIPs(ctx, hostname)
		if err != nil {
			return nil, ssrfBlocked("Blocked request - DNS resolution failed for: %s", hostname)
		}
		addrs = resolved
	}
	safe := []string{}
	for _, ip := range addrs {
		if reason := resolvedIPBlockReason(env, ip, allowPrivate); reason != "" {
			return nil, ssrfBlocked("Blocked request to %s during connect: %s -> %s", reason, hostname, ip.String())
		}
		s := ip.String()
		seen := false
		for _, have := range safe {
			if have == s {
				seen = true
			}
		}
		if !seen && len(safe) < maxSSRFConnectIPs {
			safe = append(safe, s)
		}
	}
	if len(safe) == 0 {
		return nil, ssrfBlocked("Blocked request - DNS returned no results for: %s", hostname)
	}
	return safe, nil
}

// Proxy url the environment configures for a request, nil when direct
func proxyFor(req *http.Request) *url.URL {
	u, err := http.ProxyFromEnvironment(req)
	if err != nil {
		return nil
	}
	return u
}

// HTTP client that re validates DNS at connect time and dials a vetted IP with the hostname kept for SNI
func newSSRFSafeClient(env *Env, timeout time.Duration, followRedirects bool, onRedirect func(target string) error) *http.Client {
	var proxyMu sync.Mutex
	proxyHosts := map[string]bool{}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			u := proxyFor(req)
			if u != nil {
				proxyMu.Lock()
				proxyHosts[strings.ToLower(u.Host)] = true
				proxyMu.Unlock()
			}
			return u, nil
		},
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		proxyMu.Lock()
		isProxy := proxyHosts[strings.ToLower(addr)]
		proxyMu.Unlock()
		if isProxy {
			return dialer.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		scheme := "http"
		if port == "443" {
			scheme = "https"
		}
		ips, err := resolvedHTTPConnectIPs(ctx, env, host, scheme)
		if err != nil {
			return nil, err
		}
		var last error
		for _, ip := range ips {
			conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
			if derr == nil {
				return conn, nil
			}
			last = derr
		}
		if last == nil {
			last = ssrfBlocked("Blocked request - DNS returned no usable IPs for: %s", host)
		}
		return nil, last
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !followRedirects {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		target := req.URL.String()
		if !isSafeURL(env, target) {
			return fmt.Errorf("Blocked redirect to private/internal address: %s", target)
		}
		if onRedirect != nil {
			return onRedirect(target)
		}
		return nil
	}
	return client
}

// HTTP client that vets every connect address against SSRF rules
func NewSSRFSafeClient(env *Env, timeout time.Duration, followRedirects bool) *http.Client {
	return newSSRFSafeClient(env, timeout, followRedirects, nil)
}

// Logs the fail closed safety check error and refuses the url
func safetyCheckError(log *slog.Logger, raw string, err error) bool {
	log.Warn(fmt.Sprintf("Blocked request, URL safety check error for %s: %v", raw, err))
	return false
}
