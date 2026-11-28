package web

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

// Cloud metadata host names, refused whatever the settings say
var metadataHosts = []string{"metadata.google.internal", "metadata.goog"}

// Cloud metadata addresses, refused whatever the settings say, wrapped in IPv6 or not
var metadataAddresses = func() []netip.Addr {
	var addresses []netip.Addr
	for _, ip := range []string{"169.254.169.254", "169.254.170.2", "169.254.169.253", "100.100.100.200"} {
		addresses = append(addresses, netip.MustParseAddr(ip), netip.MustParseAddr("::ffff:"+ip))
	}
	return append(addresses, netip.MustParseAddr("fd00:ec2::254"))
}()

var linkLocal = []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("::ffff:169.254.0.0/112")}

// HTTPS hosts allowed to resolve to private or benchmark space
var trustedPrivate = []string{"multimedia.nt.qq.com.cn"}

// Most vetted addresses one connection tries
const maxConnectIPs = 8

var (
	translated = netip.MustParsePrefix("::ffff:0:0:0/96")
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	// IPv6 protocol assignments, refused whole, the globally reachable assignments inside it included
	protocolAssignments = netip.MustParsePrefix("2001::/23")
)

// Address classes a fake IP range can never cover
var undeclarable = prefixes("0.0.0.0/32", "127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10",
	"::/128", "::1/128", "fc00::/7", "fe80::/10")

// IPv6 blocks the IANA special purpose registry marks not globally reachable, and the reachable blocks inside them
var (
	special6 = prefixes("::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "100:0:0:1::/64", "2001::/23", "2001:db8::/32", "2002::/16",
		"3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10")
	reachable6 = prefixes("2001:1::1/128", "2001:1::2/128", "2001:3::/32", "2001:4:112::/48", "2001:20::/28", "2001:30::/28")
)

func prefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, cidr := range cidrs {
		out[i] = netip.MustParsePrefix(cidr)
	}
	return out
}

func within(ip netip.Addr, blocks []netip.Prefix) bool {
	return slices.ContainsFunc(blocks, func(block netip.Prefix) bool { return block.Contains(ip) })
}

// IPv4 address an IPv6 wrapper reaches: mapped, or in the IPv4-translated or NAT64 well-known prefix whose low 32 bits a translator dials
func reached(ip netip.Addr) netip.Addr {
	ip = ip.Unmap().WithZone("")
	if ip.Is6() && (translated.Contains(ip) || nat64.Contains(ip)) {
		a := ip.As16()
		return netip.AddrFrom4([4]byte(a[12:16]))
	}
	return ip
}

// Whether the address is in a block that is not globally reachable, for a service that sends no key to a server of this machine or network
func Private(ip netip.Addr) bool { return private(ip) }

// Whether the address is in a block that is not globally reachable: private, loopback, link local, this network, documentation,
// benchmarking, reserved and broadcast space, and their IPv6 kin
func private(ip netip.Addr) bool {
	ip = reached(ip)
	if ip.Is4() {
		a := ip.As4()
		switch a[0] {
		case 0, 10, 127:
			return true
		case 169:
			return a[1] == 254
		case 172:
			return a[1]&0xf0 == 16
		case 192:
			return a[1] == 168 || a[1] == 0 && (a[2] == 2 || a[2] == 0 && a[3] != 9 && a[3] != 10) || a[1] == 88 && a[2] == 99 && a[3] == 2
		case 198:
			return a[1]&0xfe == 18 || a[1] == 51 && a[2] == 100
		case 203:
			return a[1] == 0 && a[2] == 113
		}
		return a[0] >= 240
	}
	return within(ip, special6) && !within(ip, reachable6)
}

// Whether the address is in space held in reserve: IPv4 240.0.0.0/4, IPv6 outside global unicast, unique local, link local and multicast
func reserved(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is4() {
		return ip.As4()[0] >= 240
	}
	a := ip.As16()
	return !(a[0]&0xe0 == 0x20 || a[0]&0xfe == 0xfc || a[0] == 0xfe && a[1]&0x80 != 0 || a[0] == 0xff)
}

// Whether the address is in the shared space 100.64.0.0/10 of carrier grade NAT
func shared(ip netip.Addr) bool {
	ip = reached(ip)
	if !ip.Is4() {
		return false
	}
	a := ip.As4()
	return a[0] == 100 && a[1]&0xc0 == 64
}

// Whether the address is a cloud metadata address or link local, refused whatever the settings say
func metadata(ip netip.Addr) bool {
	ip = reached(ip)
	return slices.Contains(metadataAddresses, ip) || within(ip, linkLocal)
}

// Whether the address is not globally reachable
func blockedIP(ip netip.Addr) bool {
	ip = reached(ip)
	return private(ip) || reserved(ip) || shared(ip) || ip.IsMulticast() || protocolAssignments.Contains(ip)
}

// Ranges a local DNS proxy answers with in place of real addresses, without those that cover an address class no proxy may claim
func fakeRanges(cfg *config.Config) []netip.Prefix {
	var ranges []netip.Prefix
	for _, entry := range cfg.GetStringSlice(webconfig.FakeIPRanges) {
		text := strings.TrimSpace(entry)
		prefix, err := netip.ParsePrefix(text)
		if err != nil {
			addr, err := netip.ParseAddr(text)
			if err != nil {
				continue
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefix = prefix.Masked()
		if !slices.ContainsFunc(undeclarable, func(block netip.Prefix) bool {
			return block.Addr().Is4() == prefix.Addr().Is4() && prefix.Overlaps(block)
		}) {
			ranges = append(ranges, prefix)
		}
	}
	return ranges
}

// Why a resolved address may not be dialed, empty when it may
func refusal(cfg *config.Config, ip netip.Addr, allowPrivate bool) string {
	if metadata(ip) {
		return "cloud metadata address"
	}
	if !allowPrivate && blockedIP(ip) && !within(reached(ip), fakeRanges(cfg)) {
		return "private/internal address"
	}
	return ""
}

// Whether the HTTPS host may resolve to private space
func trusted(hostname, scheme string) bool {
	return scheme == "https" && slices.Contains(trustedPrivate, hostname)
}

// Every address of the host name
func lookup(ctx context.Context, hostname string) ([]netip.Addr, error) {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, hostname)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, address := range addresses {
		if ip, ok := netip.AddrFromSlice(address.IP); ok {
			out = append(out, ip.Unmap().WithZone(""))
		}
	}
	return out, nil
}

// Whether a proxy is set, which resolves the names itself
func proxied() bool {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

func httpScheme(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

// Components of a URL cut at its delimiters the way RFC 3986 appendix B reads any text, nothing decoded and the scheme lower cased
type parts struct {
	scheme, authority, path, query, fragment string
}

var errHost = errors.New("invalid host")

// Cuts a URL after dropping leading controls and spaces and every tab, CR and LF; the error of an authority whose host cannot be read
func split(ref string) (parts, error) {
	ref = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, strings.TrimLeftFunc(ref, func(r rune) bool { return r <= ' ' }))
	var p parts
	ref, p.fragment, _ = strings.Cut(ref, "#")
	ref, p.query, _ = strings.Cut(ref, "?")
	if i := schemeEnd(ref); i > 0 {
		p.scheme, ref = strings.ToLower(ref[:i]), ref[i+1:]
	}
	if rest, ok := strings.CutPrefix(ref, "//"); ok {
		end := strings.IndexByte(rest, '/')
		if end < 0 {
			end = len(rest)
		}
		p.authority, ref = rest[:end], rest[end:]
	}
	p.path = ref
	if _, _, err := hostPort(p.authority); err != nil {
		return parts{}, err
	}
	if err := normalized(p.authority); err != nil {
		return parts{}, err
	}
	return p, nil
}

// Index of the colon ending a leading scheme, -1 without one
func schemeEnd(s string) int {
	for i := range len(s) {
		switch c := s[i]; {
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case '0' <= c && c <= '9' || c == '+' || c == '-' || c == '.':
			if i == 0 {
				return -1
			}
		case c == ':' && i > 0:
			return i
		default:
			return -1
		}
	}
	return -1
}

// Refuses a non ASCII authority that NFKC normalization gives URL delimiters, text another client would cut differently
func normalized(authority string) error {
	if !slices.ContainsFunc([]byte(authority), func(c byte) bool { return c >= utf8.RuneSelf }) {
		return nil
	}
	stripped := strings.Map(func(r rune) rune {
		if strings.ContainsRune("@:#?", r) {
			return -1
		}
		return r
	}, authority)
	if folded := norm.NFKC.String(stripped); folded != stripped && strings.ContainsAny(folded, "/?#@:") {
		return fmt.Errorf("%w: %q gains URL delimiters under NFKC normalization", errHost, authority)
	}
	return nil
}

// Host and port text of an authority, user info dropped and the brackets of an IP literal removed
func hostPort(authority string) (string, string, error) {
	info := authority[strings.LastIndexByte(authority, '@')+1:]
	if !strings.HasPrefix(info, "[") {
		if strings.ContainsAny(info, "[]") {
			return "", "", fmt.Errorf("%w: unbalanced or misplaced brackets in %q", errHost, info)
		}
		host, port, _ := strings.Cut(info, ":")
		return host, port, nil
	}
	end := strings.IndexByte(info, ']')
	if end < 0 {
		return "", "", fmt.Errorf("%w: unbalanced or misplaced brackets in %q", errHost, info)
	}
	host, rest := info[1:end], info[end+1:]
	port := ""
	if rest != "" {
		if rest[0] != ':' {
			return "", "", fmt.Errorf("%w: text after the closing bracket in %q", errHost, info)
		}
		port = rest[1:]
	}
	if host != "" && (host[0] == 'v' || host[0] == 'V') {
		dot := strings.IndexByte(host, '.')
		if dot < 2 || dot == len(host)-1 || strings.IndexFunc(host[1:dot], func(r rune) bool { return !strings.ContainsRune("0123456789abcdefABCDEF", r) }) >= 0 {
			return "", "", fmt.Errorf("%w: %q is not a valid IPvFuture literal", errHost, host)
		}
		return host, port, nil
	}
	addr, err := netip.ParseAddr(host)
	if _, zone, _ := strings.Cut(host, "%"); err != nil || strings.Contains(zone, "%") {
		return "", "", fmt.Errorf("%w: %q is not an IPv6 address", errHost, host)
	}
	if addr.Is4() {
		return "", "", fmt.Errorf("%w: IPv4 address %q in brackets", errHost, host)
	}
	return host, port, nil
}

func (p parts) hostname() string {
	host, _, _ := hostPort(p.authority)
	if h, zone, ok := strings.Cut(host, "%"); ok {
		return strings.ToLower(h) + "%" + zone
	}
	return strings.ToLower(host)
}

// Whether the authority names a port that is no decimal number up to 65535
func (p parts) badPort() bool {
	_, port, err := hostPort(p.authority)
	if err != nil {
		return true
	}
	if port == "" {
		return false
	}
	n := 0
	for i := range len(port) {
		if port[i] < '0' || port[i] > '9' {
			return true
		}
		if n <= 65535 {
			n = n*10 + int(port[i]-'0')
		}
	}
	return n > 65535
}

const edges = " \t\n\r\x0b\x0c"

// Dots IDNA maps to a period, for a name IDNA cannot encode
var dots = strings.NewReplacer("。", ".", "．", ".", "｡", ".")

// Host a client dials for the URL, in ASCII and lower case without trailing dots
func hostOf(raw string) (string, error) {
	p, err := split(strings.Trim(raw, edges))
	if err != nil {
		return "", err
	}
	host := p.hostname()
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		host = ascii
	} else if ascii, err := idna.Punycode.ToASCII(host); err == nil && ascii != "" {
		host = ascii
	} else {
		host = dots.Replace(host)
	}
	return bare(host), nil
}

func bare(host string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(host)), ".")
}

// Why another client could read a different host or port from an http URL, empty when none could. The floor checks only what moves the
// host
func ambiguity(raw string, floor bool) (string, error) {
	raw = strings.Trim(raw, edges)
	p, err := split(raw)
	if err != nil {
		return "", err
	}
	switch {
	case !httpScheme(p.scheme):
		return "", nil
	case strings.ContainsFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "control character in URL", nil
	case p.authority == "":
		return "authority not introduced by //", nil
	case strings.Contains(p.authority, `\`):
		return "backslash in authority", nil
	case floor:
		return "", nil
	case strings.Contains(p.authority, "@"):
		return "userinfo in authority", nil
	case strings.Contains(p.authority, "%"):
		return "percent-encoding in host", nil
	case strings.ContainsFunc(p.authority, unicode.IsSpace):
		return "whitespace in host", nil
	case p.badPort():
		return "invalid port", nil
	}
	return "", nil
}

// IP address a host name spells, false for a name
func literal(hostname string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(hostname, "["), "]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.WithZone(""), true
}

// Whether the URL is http or https on a public host: not ambiguous, not cloud metadata, and resolving to no private or internal address
// unless Read private addresses allows those. Anything that cannot be checked is refused, but a name that does not resolve passes through a
// proxy, which resolves it itself
func SafeURL(cfg *config.Config, raw string) bool {
	return SafeFor(cfg, raw, cfg.GetBool(webconfig.PrivateURLs))
}

// SafeURL with private and internal addresses allowed by the caller's own switch, such as the one of the browser
func SafeFor(cfg *config.Config, raw string, private bool) bool {
	p, err := split(raw)
	if err != nil || !httpScheme(p.scheme) {
		return false
	}
	if reason, err := ambiguity(raw, false); err != nil || reason != "" {
		return false
	}
	hostname, err := hostOf(raw)
	if err != nil || hostname == "" || slices.Contains(metadataHosts, hostname) {
		return false
	}
	allow := private || trusted(hostname, p.scheme)
	addresses := []netip.Addr{}
	if ip, ok := literal(hostname); ok {
		addresses = append(addresses, ip)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resolved, err := lookup(ctx, hostname)
		if err != nil {
			return proxied()
		}
		addresses = resolved
	}
	return !slices.ContainsFunc(addresses, func(ip netip.Addr) bool { return refusal(cfg, ip, allow) != "" })
}

// Why a URL whose host is private or internal is not read
const BlockedPrivate = "Blocked: URL targets a private or internal network address"

// Refusal of a connection to a private or internal address
type BlockedError struct {
	Message string
}

func (e *BlockedError) Error() string { return e.Message }

// Addresses of the host checked when a connection is made, so a name that resolves differently after SafeURL is still refused
func connectIPs(ctx context.Context, cfg *config.Config, host, scheme string) ([]string, error) {
	hostname := bare(host)
	switch {
	case hostname == "":
		return nil, &BlockedError{"Blocked request with empty hostname"}
	case slices.Contains(metadataHosts, hostname):
		return nil, &BlockedError{"Blocked request to internal hostname: " + hostname}
	}
	allow := cfg.GetBool(webconfig.PrivateURLs) || trusted(hostname, scheme)
	var addresses []netip.Addr
	if ip, ok := literal(hostname); ok {
		addresses = []netip.Addr{ip}
	} else {
		resolved, err := lookup(ctx, hostname)
		if err != nil {
			return nil, &BlockedError{"Blocked request - DNS resolution failed for: " + hostname}
		}
		addresses = resolved
	}
	var safe []string
	for _, ip := range addresses {
		if reason := refusal(cfg, ip, allow); reason != "" {
			return nil, &BlockedError{fmt.Sprintf("Blocked request to %s during connect: %s -> %s", reason, hostname, ip)}
		}
		if text := ip.String(); !slices.Contains(safe, text) && len(safe) < maxConnectIPs {
			safe = append(safe, text)
		}
	}
	if len(safe) == 0 {
		return nil, &BlockedError{"Blocked request - DNS returned no results for: " + hostname}
	}
	return safe, nil
}

// HTTP client that checks every address it connects to against the same rules as SafeURL, dialing a checked address with the host name
// kept for TLS, and every redirect target before following it. A proxy the environment sets is dialed as it is
func Client(cfg *config.Config, timeout time.Duration, follow bool) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	var mu sync.Mutex
	proxies := map[string]bool{}
	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			proxy, err := http.ProxyFromEnvironment(req)
			if err != nil || proxy == nil {
				return nil, nil
			}
			mu.Lock()
			proxies[strings.ToLower(proxy.Host)] = true
			mu.Unlock()
			return proxy, nil
		},
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		proxy := proxies[strings.ToLower(address)]
		mu.Unlock()
		if proxy {
			return dialer.DialContext(ctx, network, address)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		scheme := "http"
		if port == "443" {
			scheme = "https"
		}
		ips, err := connectIPs(ctx, cfg, host, scheme)
		if err != nil {
			return nil, err
		}
		var last error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		switch {
		case !follow:
			return http.ErrUseLastResponse
		case len(via) >= 10:
			return errors.New("stopped after 10 redirects")
		case !SafeURL(cfg, req.URL.String()):
			return &BlockedError{"Blocked redirect to private/internal address: " + req.URL.String()}
		}
		return nil
	}}
}

// URL as a request sends it: the host in IDNA ASCII and text outside ASCII in the path, query and fragment percent encoded. Anything but an
// http URL comes back trimmed and otherwise as it was
func NormalizeURL(raw string) string {
	raw = schemeSpace.ReplaceAllString(strings.TrimSpace(raw), "$1")
	if raw == "" {
		return raw
	}
	scheme, netloc, path, query, fragment := cut(raw)
	if !httpScheme(strings.ToLower(scheme)) {
		return raw
	}
	if host := netlocHost(netloc); host != "" {
		original := host
		if i := strings.Index(strings.ToLower(netloc), host); i >= 0 {
			original = netloc[i : i+len(host)]
		}
		if ascii, err := idna.Lookup.ToASCII(original); err == nil && ascii != original {
			netloc = strings.Replace(netloc, original, ascii, 1)
		}
	}
	const safe = "/%:@!$&'()*+,;="
	out := scheme + "://" + netloc + quote(path, safe)
	if query != "" || strings.Contains(raw, "?") {
		out += "?" + quote(query, safe+"?")
	}
	if fragment != "" || strings.Contains(raw, "#") {
		out += "#" + quote(fragment, safe+"?")
	}
	return out
}

var schemeSpace = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.\-]*://)\s+`)

var schemeChars = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*$`)

// Scheme, authority, path, query and fragment of a URL, the scheme as written
func cut(raw string) (scheme, netloc, path, query, fragment string) {
	rest := raw
	if i := strings.Index(rest, ":"); i > 0 && schemeChars.MatchString(rest[:i]) {
		scheme, rest = rest[:i], rest[i+1:]
	}
	if after, ok := strings.CutPrefix(rest, "//"); ok {
		end := strings.IndexAny(after, "/?#")
		if end < 0 {
			end = len(after)
		}
		netloc, rest = after[:end], after[end:]
	}
	rest, fragment, _ = strings.Cut(rest, "#")
	path, query, _ = strings.Cut(rest, "?")
	return scheme, netloc, path, query, fragment
}

// Host of an authority, lower cased, user info and port dropped and an IPv6 literal unbracketed
func netlocHost(netloc string) string {
	host := netloc[strings.LastIndex(netloc, "@")+1:]
	if strings.HasPrefix(host, "[") {
		if end := strings.Index(host, "]"); end >= 0 {
			return strings.ToLower(host[1:end])
		}
		return strings.ToLower(host[1:])
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return strings.ToLower(host)
}

// Percent encodes every byte outside the unreserved set and the safe set
func quote(s, safe string) string {
	var b strings.Builder
	for i := range len(s) {
		switch c := s[i]; {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', strings.IndexByte("_.-~", c) >= 0, strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
