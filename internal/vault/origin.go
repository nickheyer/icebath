package vault

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Characters a URL scheme may hold after its leading letter
const schemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

// ASCII controls and space stripped from the front of a URL before it is split
const leadingJunk = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "

// Bracketed future address grammar, a v, hex digits, a dot and anything after
var ipvFuture = regexp.MustCompile(`^v[a-fA-F0-9]+\..+$`)

// Bytes removed from anywhere in a URL before it is split
var urlNoise = strings.NewReplacer("\t", "", "\r", "", "\n", "")

// Characters dropped before the NFKC netloc check
var netlocDelimiters = strings.NewReplacer("@", "", ":", "", "#", "", "?", "")

// Scheme, network location and query of a URL
type urlParts struct {
	scheme string
	netloc string
	query  string
}

// Port a scheme implies when the URL names none
func defaultPort(scheme string) (int, bool) {
	switch scheme {
	case "http":
		return 80, true
	case "https":
		return 443, true
	}
	return 0, false
}

// Origin of a URL or origin as scheme, host and a non-default port, the form items bind and pages match
func NormalizeOrigin(urlOrOrigin string) (string, error) {
	value := trimSpace(urlOrOrigin)
	if value == "" {
		return "", failure(ErrInvalid, "origin is required")
	}
	if !strings.Contains(value, "://") {
		return "", failure(ErrInvalid, "origin must include a scheme (got %s)", quote(value))
	}
	parts, err := splitURL(value)
	if err != nil {
		return "", err
	}
	host, port := parts.hostPort()
	host = lowerFull(host)
	if parts.scheme == "" || host == "" {
		return "", failure(ErrInvalid, "could not parse origin from %s", quote(value))
	}
	if port == "" {
		return parts.scheme + "://" + host, nil
	}
	n, ok := portNumber(port)
	if !ok {
		return "", failure(ErrInvalid, "invalid port in origin %s", quote(value))
	}
	if def, known := defaultPort(parts.scheme); known && n == def {
		return parts.scheme + "://" + host, nil
	}
	return parts.scheme + "://" + host + ":" + strconv.Itoa(n), nil
}

// Splits raw into scheme, network location and query, refusing malformed brackets and NFKC netloc tricks
func splitURL(raw string) (urlParts, error) {
	url := urlNoise.Replace(strings.TrimLeft(raw, leadingJunk))
	var p urlParts
	if i := strings.IndexByte(url, ':'); i > 0 && isASCIILetter(url[0]) && strings.Trim(url[:i], schemeChars) == "" {
		p.scheme, url = strings.ToLower(url[:i]), url[i+1:]
	}
	if rest, ok := strings.CutPrefix(url, "//"); ok {
		end := len(rest)
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			end = j
		}
		p.netloc, url = rest[:end], rest[end:]
		open, closed := strings.Contains(p.netloc, "["), strings.Contains(p.netloc, "]")
		if open != closed {
			return p, failure(ErrInvalid, "Invalid IPv6 URL")
		}
		if open {
			if err := checkBracketedNetloc(p.netloc); err != nil {
				return p, err
			}
		}
	}
	if before, _, found := strings.Cut(url, "#"); found {
		url = before
	}
	if _, query, found := strings.Cut(url, "?"); found {
		p.query = query
	}
	if err := checkNetloc(p.netloc); err != nil {
		return p, err
	}
	return p, nil
}

// Reports whether c is an ASCII letter
func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// Refuses text before an opening bracket or between closing bracket and port, then checks the implied host
func checkBracketedNetloc(netloc string) error {
	hostPort := netloc[strings.LastIndexByte(netloc, '@')+1:]
	before, bracketed, found := strings.Cut(hostPort, "[")
	if !found {
		host, _, _ := strings.Cut(hostPort, ":")
		return checkBracketedHost(host)
	}
	if before != "" {
		return failure(ErrInvalid, "Invalid IPv6 URL")
	}
	host, port, _ := strings.Cut(bracketed, "]")
	if port != "" && !strings.HasPrefix(port, ":") {
		return failure(ErrInvalid, "Invalid IPv6 URL")
	}
	return checkBracketedHost(host)
}

// Refuses a bracketed host that is neither a valid IPvFuture literal nor an IPv6 address with at most one zone
func checkBracketedHost(host string) error {
	if strings.HasPrefix(host, "v") {
		if !ipvFuture.MatchString(host) {
			return failure(ErrInvalid, "IPvFuture address is invalid")
		}
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if _, zone, zoned := strings.Cut(host, "%"); err != nil || zoned && strings.Contains(zone, "%") {
		return failure(ErrInvalid, "%s does not appear to be an IPv4 or IPv6 address", quote(host))
	}
	if addr.Is4() {
		return failure(ErrInvalid, "An IPv4 address cannot be in brackets")
	}
	return nil
}

// Refuses a non-ASCII netloc whose NFKC form gains a delimiter it did not visibly hold
func checkNetloc(netloc string) error {
	if netloc == "" || isASCII(netloc) {
		return nil
	}
	n := netlocDelimiters.Replace(netloc)
	folded := norm.NFKC.String(n)
	if folded != n && strings.ContainsAny(folded, "/?#@:") {
		return failure(ErrInvalid, "netloc '%s' contains invalid characters under NFKC normalization", netloc)
	}
	return nil
}

// Host and port text after any userinfo, a bracketed host taken without its brackets
func (p urlParts) hostPort() (host, port string) {
	info := p.netloc[strings.LastIndexByte(p.netloc, '@')+1:]
	if _, bracketed, found := strings.Cut(info, "["); found {
		inner, rest, _ := strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(rest, ":")
		return inner, port
	}
	host, port, _ = strings.Cut(info, ":")
	return host, port
}

// Port number of ASCII decimal text in range 0 to 65535
func portNumber(text string) (int, bool) {
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return 0, false
		}
	}
	digits := strings.TrimLeft(text, "0")
	if len(digits) > 5 {
		return 0, false
	}
	if digits == "" {
		return 0, true
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil && n <= 65535
}

// First value of each query name with names lowercased, a later spelling of a name winning
func queryFirsts(query string) map[string]string {
	type pair struct{ name, value string }
	var firsts []pair
	seen := map[string]bool{}
	for _, field := range strings.Split(query, "&") {
		name, value, found := strings.Cut(field, "=")
		if !found || value == "" {
			continue
		}
		name, value = unquotePlus(name), unquotePlus(value)
		if !seen[name] {
			seen[name] = true
			firsts = append(firsts, pair{name, value})
		}
	}
	out := make(map[string]string, len(firsts))
	for _, f := range firsts {
		out[lowerFull(f.name)] = f.value
	}
	return out
}

// Query text with plus as space and percent escapes decoded per ASCII run as UTF-8
func unquotePlus(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for len(s) > 0 {
		end := 0
		for end < len(s) && s[end] < 0x80 {
			end++
		}
		if end == 0 {
			for end < len(s) && s[end] >= 0x80 {
				end++
			}
			b.WriteString(s[:end])
		} else {
			b.WriteString(lossyUTF8(percentDecode(s[:end])))
		}
		s = s[end:]
	}
	return b.String()
}

// Bytes of ASCII text with every percent sign followed by two hex digits decoded
func percentDecode(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			out = append(out, hexValue(s[i+1])<<4|hexValue(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return out
}

// Reports whether c is a hex digit
func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// Value of the hex digit c
func hexValue(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}
