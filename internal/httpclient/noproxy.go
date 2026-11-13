package httpclient

import (
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Both spellings of the bypass variable, read and written together
var noProxyKeys = []string{"NO_PROXY", "no_proxy"}

// Hosts appended to a child bypass list so local endpoints never go through a proxy
var loopbackHosts = []string{"127.0.0.1", "localhost", "::1"}

// Entries of a NO_PROXY value split on commas and whitespace
func NoProxyEntries(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}

// Reports whether the host is localhost or any loopback address literal, IPv4 mapped forms included
func IsLoopbackHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]")), ".")
	if h == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.WithZone("").Unmap().IsLoopback()
}

// Child environment with the loopback hosts appended to both bypass spellings, a wildcard list left alone
func LoopbackNoProxy(environ []string) []string {
	env := envList(environ)
	for _, key := range noProxyKeys {
		value, _ := env(key)
		if slices.Contains(NoProxyEntries(value), "*") {
			return slices.Clone(environ)
		}
	}
	out := slices.Clone(environ)
	for _, key := range noProxyKeys {
		value, _ := env(key)
		entries := NoProxyEntries(value)
		added := false
		for _, host := range loopbackHosts {
			if !slices.Contains(entries, host) {
				entries = append(entries, host)
				added = true
			}
		}
		if added {
			out = setEnv(out, key, strings.Join(entries, ","))
		}
	}
	return out
}

// Child environment with proxy URLs trimmed, socks made socks5, and bracketed or CIDR IPv6 bypass entries made bare
func CanonicalProxyEnv(environ []string) []string {
	env := envList(environ)
	out := slices.Clone(environ)
	for _, key := range anySchemeKeys {
		if value, ok := env(key); ok {
			if normalized := normalizeProxy(value); normalized != "" && normalized != value {
				out = setEnv(out, key, normalized)
			}
		}
	}
	for _, key := range noProxyKeys {
		if value, ok := env(key); ok && value != "" {
			if sanitized := sanitizeNoProxy(value); sanitized != value {
				out = setEnv(out, key, sanitized)
			}
		}
	}
	return out
}

// Bypass value with IPv6 entries in brackets or with a prefix length reduced to bare literals, unchanged otherwise
func sanitizeNoProxy(value string) string {
	entries := NoProxyEntries(value)
	if len(entries) == 0 || slices.Contains(entries, "*") {
		return value
	}
	rewritten := make([]string, 0, len(entries))
	for _, entry := range entries {
		bare, ok := bareIPv6(entry)
		switch {
		case !ok:
			rewritten = append(rewritten, entry)
		case !slices.Contains(rewritten, bare):
			rewritten = append(rewritten, bare)
		}
	}
	if slices.Equal(rewritten, entries) {
		return value
	}
	return strings.Join(rewritten, ",")
}

// Bare IPv6 literal of a bracketed entry with optional suffix or an unbracketed entry with a prefix length
func bareIPv6(entry string) (string, bool) {
	candidate := strings.TrimSpace(entry)
	var literal string
	if strings.HasPrefix(candidate, "[") {
		end := strings.Index(candidate, "]")
		if end < 0 {
			return "", false
		}
		literal = candidate[1:end]
		if rest := candidate[end+1:]; rest != "" && rest[0] != '/' && rest[0] != ':' {
			return "", false
		}
	} else {
		addr, bits, ok := strings.Cut(candidate, "/")
		if !ok || !allDigits(bits) {
			return "", false
		}
		literal = addr
	}
	if literal == "" || strings.Trim(literal, "0123456789abcdefABCDEF:.") != "" {
		return "", false
	}
	ip, err := netip.ParseAddr(literal)
	if err != nil || !ip.Is6() {
		return "", false
	}
	return literal, true
}

// One parsed bypass entry, a network for address literals and CIDR ranges or a domain suffix otherwise
type bypassRule struct {
	port    int
	network netip.Prefix
	suffix  string
}

// Parsed bypass list where a lone star bypasses every host
type bypassList struct {
	all   bool
	rules []bypassRule
}

// Bypass list from both spellings of NO_PROXY in the process environment
func envBypass() bypassList {
	values := make([]string, 0, len(noProxyKeys))
	for _, key := range noProxyKeys {
		values = append(values, os.Getenv(key))
	}
	return parseBypass(strings.Join(values, ","))
}

// Rules of a bypass value covering hosts, suffixes, ports, address literals, CIDR ranges and wildcards
func parseBypass(value string) bypassList {
	var list bypassList
	for _, entry := range NoProxyEntries(value) {
		token := strings.ToLower(strings.TrimSpace(entry))
		if token == "*" {
			list.all = true
			continue
		}
		host, port := splitBypassHost(token)
		if host == "" {
			continue
		}
		rule := bypassRule{port: port}
		if network, ok := parseNetwork(host); ok {
			rule.network = network
		} else {
			rule.suffix = strings.TrimPrefix(strings.TrimPrefix(host, "*"), ".")
			if rule.suffix == "" {
				continue
			}
		}
		list.rules = append(list.rules, rule)
	}
	return list
}

// Reports whether the host on the port matches an entry, port minus one meaning unknown
func (l bypassList) match(host string, port int) bool {
	host = strings.TrimRight(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	if l.all {
		return true
	}
	ip, ipErr := netip.ParseAddr(host)
	for _, rule := range l.rules {
		if rule.port >= 0 && rule.port != port {
			continue
		}
		if rule.network.IsValid() {
			if ipErr == nil && rule.network.Contains(ip.WithZone("")) {
				return true
			}
			continue
		}
		if host == rule.suffix || strings.HasSuffix(host, "."+rule.suffix) {
			return true
		}
	}
	return false
}

// Host and port of a bypass entry given as a URL, bracketed literal, host and port, or bare host
func splitBypassHost(raw string) (string, int) {
	port := -1
	var host string
	switch {
	case strings.Contains(raw, "://") || strings.HasPrefix(raw, "//"):
		rest := raw[strings.Index(raw, "//")+2:]
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			rest = rest[:i]
		}
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			rest = rest[i+1:]
		}
		var portText string
		if strings.HasPrefix(rest, "[") {
			inner, after, _ := strings.Cut(rest[1:], "]")
			host = inner
			portText, _ = strings.CutPrefix(after, ":")
		} else {
			host, portText, _ = strings.Cut(rest, ":")
		}
		if n, ok := portNumber(portText); ok {
			port = n
		}
	case strings.HasPrefix(raw, "[") && strings.Contains(raw, "]"):
		inner, rest, _ := strings.Cut(raw[1:], "]")
		host = inner
		if text, ok := strings.CutPrefix(rest, ":"); ok && allDigits(text) {
			port, _ = strconv.Atoi(text)
		}
	case strings.Count(raw, ":") == 1 && allDigits(raw[strings.LastIndex(raw, ":")+1:]):
		i := strings.LastIndex(raw, ":")
		host = raw[:i]
		port, _ = strconv.Atoi(raw[i+1:])
	default:
		host = strings.Trim(raw, "[]")
	}
	return strings.TrimRight(strings.ToLower(host), "."), port
}

// Network of an address literal, a CIDR range, or an IPv4 address with a dotted net or host mask
func parseNetwork(host string) (netip.Prefix, bool) {
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.WithZone("")
		return netip.PrefixFrom(ip, ip.BitLen()), true
	}
	addrText, maskText, ok := strings.Cut(host, "/")
	if !ok {
		return netip.Prefix{}, false
	}
	ip, err := netip.ParseAddr(addrText)
	if err != nil || ip.Zone() != "" {
		return netip.Prefix{}, false
	}
	if allDigits(maskText) {
		bits, err := strconv.Atoi(maskText)
		if err != nil || bits > ip.BitLen() {
			return netip.Prefix{}, false
		}
		return netip.PrefixFrom(ip, bits).Masked(), true
	}
	mask, err := netip.ParseAddr(maskText)
	if err != nil || !ip.Is4() || !mask.Is4() {
		return netip.Prefix{}, false
	}
	b := mask.As4()
	value := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	if bits, ok := contiguousBits(value); ok {
		return netip.PrefixFrom(ip, bits).Masked(), true
	}
	if bits, ok := contiguousBits(^value); ok {
		return netip.PrefixFrom(ip, bits).Masked(), true
	}
	return netip.Prefix{}, false
}

// Prefix length of a netmask whose set bits are all leading
func contiguousBits(mask uint32) (int, bool) {
	bits := 0
	for bits < 32 && mask&(1<<(31-bits)) != 0 {
		bits++
	}
	if bits < 32 && mask<<bits != 0 {
		return 0, false
	}
	return bits, true
}

// Port of a URL authority when it is a decimal number up to 65535
func portNumber(text string) (int, bool) {
	if text == "" || !allDigits(text) {
		return 0, false
	}
	n, err := strconv.Atoi(text)
	if err != nil || n > 65535 {
		return 0, false
	}
	return n, true
}

// Reports whether text is a non-empty run of ASCII digits
func allDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// Lowercased host and effective port of a target, ws and wss counting as http and https
func targetHostPort(u *url.URL) (string, int) {
	host, port := bypassTarget(u)
	if port >= 0 {
		return host, port
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		return host, 443
	case "http", "ws":
		return host, 80
	}
	return host, -1
}

// Lowercased host and the port written in the URL, minus one when the URL names none
func bypassTarget(u *url.URL) (string, int) {
	host := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if n, ok := portNumber(u.Port()); ok {
		return host, n
	}
	return host, -1
}
