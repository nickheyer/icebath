package mcp

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// pyURL holds the parts urllib.parse splits a URL into, params apart from the path
type pyURL struct {
	Scheme, Netloc, Path, Params, Query, Fragment string
}

var errInvalidIPv6URL = errors.New("Invalid IPv6 URL")

// Splits like urllib.parse.urlsplit including its control stripping and bracket checks
func pyURLSplit(raw string) (pyURL, error) {
	var u pyURL
	raw = strings.TrimLeft(raw, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)
	if i := strings.IndexByte(raw, ':'); i > 0 && raw[0] < utf8.RuneSelf && isASCIIAlpha(raw[0]) {
		ok := true
		for j := 0; j < i; j++ {
			c := raw[j]
			if !(isASCIIAlpha(c) || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.') {
				ok = false
				break
			}
		}
		if ok {
			u.Scheme = strings.ToLower(raw[:i])
			raw = raw[i+1:]
		}
	}
	if strings.HasPrefix(raw, "//") {
		end := len(raw)
		for _, c := range "/?#" {
			if k := strings.IndexRune(raw[2:], c); k >= 0 && k+2 < end {
				end = k + 2
			}
		}
		u.Netloc, raw = raw[2:end], raw[end:]
		open, closed := strings.Contains(u.Netloc, "["), strings.Contains(u.Netloc, "]")
		if open != closed {
			return pyURL{}, errInvalidIPv6URL
		}
		if open && closed {
			if err := checkBracketedNetloc(u.Netloc); err != nil {
				return pyURL{}, err
			}
		}
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw, u.Fragment = raw[:i], raw[i+1:]
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw, u.Query = raw[:i], raw[i+1:]
	}
	u.Path = raw
	return u, nil
}

// Validates the bracketed host of a netloc the way urlsplit checks it
func checkBracketedNetloc(netloc string) error {
	hostinfo := netloc[strings.LastIndex(netloc, "@")+1:]
	open := strings.Index(hostinfo, "[")
	closeIdx := strings.Index(hostinfo, "]")
	if open < 0 || closeIdx < open {
		return errInvalidIPv6URL
	}
	host := hostinfo[open+1 : closeIdx]
	if strings.HasPrefix(host, "v") {
		if len(host) < 2 || !strings.Contains(host, ".") {
			return fmt.Errorf("IPvFuture address is invalid")
		}
		return nil
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.Is6() {
		return fmt.Errorf("%q does not appear to be an IPv4 or IPv6 address", host)
	}
	return nil
}

var usesParams = map[string]bool{"": true, "ftp": true, "hdl": true, "prospero": true, "http": true, "imap": true, "https": true,
	"shttp": true, "rtsp": true, "rtsps": true, "rtspu": true, "sip": true, "sips": true, "mms": true, "sftp": true, "tel": true}

var usesRelative = map[string]bool{"": true, "ftp": true, "http": true, "gopher": true, "nntp": true, "imap": true, "wais": true,
	"file": true, "https": true, "shttp": true, "mms": true, "prospero": true, "rtsp": true, "rtsps": true, "rtspu": true,
	"sftp": true, "svn": true, "svn+ssh": true, "ws": true, "wss": true}

// Parses like urllib.parse.urlparse, moving the last segment's params out of the path
func pyURLParse(raw string) (pyURL, error) {
	u, err := pyURLSplit(raw)
	if err != nil {
		return u, err
	}
	if usesParams[u.Scheme] && strings.Contains(u.Path, ";") {
		start := strings.LastIndex(u.Path, "/")
		if start < 0 {
			start = 0
		}
		if i := strings.IndexByte(u.Path[start:], ';'); i >= 0 {
			u.Path, u.Params = u.Path[:start+i], u.Path[start+i+1:]
		}
	}
	return u, nil
}

// Hostinfo of a netloc after any userinfo
func netlocHostinfo(netloc string) string { return netloc[strings.LastIndex(netloc, "@")+1:] }

// Lowercased hostname of a netloc the way urlparse's hostname property derives it, empty when absent
func netlocHostname(netloc string) string {
	hostinfo := netlocHostinfo(netloc)
	var host string
	if i := strings.Index(hostinfo, "["); i >= 0 {
		rest := hostinfo[i+1:]
		if j := strings.Index(rest, "]"); j >= 0 {
			host = rest[:j]
		}
	} else {
		host, _, _ = strings.Cut(hostinfo, ":")
	}
	if host == "" {
		return ""
	}
	if i := strings.IndexByte(host, '%'); i >= 0 && strings.Contains(host, ":") {
		return strings.ToLower(host[:i]) + host[i:]
	}
	return strings.ToLower(host)
}

// Port of a netloc the way urlparse's port property reads it, error where Python raises
func netlocPort(netloc string) (int, bool, error) {
	hostinfo := netlocHostinfo(netloc)
	var port string
	if i := strings.Index(hostinfo, "["); i >= 0 {
		rest := hostinfo[i:]
		if j := strings.Index(rest, "]"); j >= 0 {
			_, port, _ = strings.Cut(rest[j:], ":")
		}
	} else {
		_, port, _ = strings.Cut(hostinfo, ":")
	}
	if port == "" {
		return 0, false, nil
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return 0, false, fmt.Errorf("Port could not be cast to integer value as %s", pyReprStr(port))
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n > 65535 {
		return 0, false, errors.New("Port out of range 0-65535")
	}
	return n, true, nil
}

// Reports whether a netloc carries a username or password
func netlocHasUserinfo(netloc string) bool { return strings.Contains(netloc, "@") }

// Joins an absolute path reference onto a base like urllib.parse.urljoin, removing dot segments
func pyURLJoinAbsPath(base, ref string) string {
	if base == "" {
		return ref
	}
	b, err := pyURLParse(base)
	if err != nil {
		return ref
	}
	r, err := pyURLParse(ref)
	if err != nil {
		return ref
	}
	if r.Scheme != "" && r.Scheme != b.Scheme || !usesRelative[b.Scheme] {
		return ref
	}
	if r.Netloc != "" {
		return pyURLUnparse(pyURL{Scheme: b.Scheme, Netloc: r.Netloc, Path: r.Path, Params: r.Params, Query: r.Query, Fragment: r.Fragment})
	}
	segments := strings.Split(r.Path, "/")
	var resolved []string
	for _, seg := range segments {
		switch seg {
		case "..":
			if len(resolved) > 0 {
				resolved = resolved[:len(resolved)-1]
			}
		case ".":
		default:
			resolved = append(resolved, seg)
		}
	}
	if last := segments[len(segments)-1]; last == "." || last == ".." {
		resolved = append(resolved, "")
	}
	p := strings.Join(resolved, "/")
	if p == "" {
		p = "/"
	}
	return pyURLUnparse(pyURL{Scheme: b.Scheme, Netloc: b.Netloc, Path: p, Params: r.Params, Query: r.Query, Fragment: r.Fragment})
}

// Reassembles parts like urllib.parse.urlunparse
func pyURLUnparse(u pyURL) string {
	p := u.Path
	if u.Params != "" {
		p += ";" + u.Params
	}
	var b strings.Builder
	if u.Netloc != "" || (u.Scheme != "" && strings.HasPrefix(p, "//")) {
		if p != "" && !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		b.WriteString("//" + u.Netloc)
	}
	b.WriteString(p)
	out := b.String()
	if u.Scheme != "" {
		out = u.Scheme + ":" + out
	}
	if u.Query != "" {
		out += "?" + u.Query
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return out
}

// Scheme and netloc of a URL joined as urlparse reports them
func pyURLOrigin(raw string) string {
	u, _ := pyURLParse(raw)
	return u.Scheme + "://" + u.Netloc
}

// Decodes percent escapes like urllib.parse.unquote with invalid UTF-8 replaced
func pyUnquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) {
			v, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			out = append(out, byte(v))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return decodeReplace(out)
}

// Parses a query string like urllib.parse.parse_qs, blank values kept only on request
func pyParseQSL(query string, keepBlank bool) [][2]string {
	var out [][2]string
	for _, part := range strings.Split(query, "&") {
		if part == "" {
			continue
		}
		name, value, found := strings.Cut(part, "=")
		if !found {
			if !keepBlank {
				continue
			}
		}
		if value == "" && !keepBlank {
			continue
		}
		out = append(out, [2]string{pyUnquote(strings.ReplaceAll(name, "+", " ")), pyUnquote(strings.ReplaceAll(value, "+", " "))})
	}
	return out
}

const pyAlwaysSafe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~"

// Percent encodes like urllib.parse.quote with the given extra safe characters
func pyQuote(s, safe string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(pyAlwaysSafe, c) >= 0 || strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// Percent encodes like urllib.parse.quote_plus, spaces becoming plus signs
func pyQuotePlus(s string) string {
	return strings.ReplaceAll(pyQuote(s, " "), " ", "+")
}

// Encodes ordered pairs like urllib.parse.urlencode
func pyURLEncode(pairs [][2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, pyQuotePlus(p[0])+"="+pyQuotePlus(p[1]))
	}
	return strings.Join(parts, "&")
}

func isASCIIAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

var specialSchemePorts = map[string]string{"http": "80", "https": "443", "ws": "80", "wss": "443", "ftp": "21", "file": ""}

// urlError is a URL parse failure in the pydantic error vocabulary
type urlError struct{ typ, msg string }

func (e *urlError) Error() string { return e.msg }

// Serializes a URL the way pydantic's WHATWG based Url type renders it, empty paths preserved
func normalizeURL(raw string, httpOnly bool) (string, error) {
	raw = strings.Trim(raw, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	raw = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(raw)
	if raw == "" {
		return "", &urlError{"url_parsing", "Input should be a valid URL, input is empty"}
	}
	i := strings.IndexByte(raw, ':')
	if i <= 0 || !isASCIIAlpha(raw[0]) {
		return "", &urlError{"url_parsing", "Input should be a valid URL, relative URL without a base"}
	}
	for j := 0; j < i; j++ {
		c := raw[j]
		if !(isASCIIAlpha(c) || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.') {
			return "", &urlError{"url_parsing", "Input should be a valid URL, relative URL without a base"}
		}
	}
	scheme := strings.ToLower(raw[:i])
	rest := raw[i+1:]
	if httpOnly && scheme != "http" && scheme != "https" {
		return "", &urlError{"url_scheme", "URL scheme should be 'http' or 'https'"}
	}
	defPort, special := specialSchemePorts[scheme]
	if !special {
		return scheme + ":" + rest, nil
	}
	rest = strings.ReplaceAll(rest, "\\", "/")
	rest = strings.TrimLeft(rest, "/")
	end := len(rest)
	if k := strings.IndexAny(rest, "/?#"); k >= 0 {
		end = k
	}
	authority, tail := rest[:end], rest[end:]
	userinfo := ""
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo, authority = authority[:at], authority[at+1:]
	}
	hostPart, port := authority, ""
	if strings.HasPrefix(authority, "[") {
		closeIdx := strings.Index(authority, "]")
		if closeIdx < 0 {
			return "", &urlError{"url_parsing", "Input should be a valid URL, invalid IPv6 address"}
		}
		hostPart = authority[:closeIdx+1]
		if after := authority[closeIdx+1:]; after != "" {
			if !strings.HasPrefix(after, ":") {
				return "", &urlError{"url_parsing", "Input should be a valid URL, invalid IPv6 address"}
			}
			port = after[1:]
		}
	} else if k := strings.LastIndex(authority, ":"); k >= 0 {
		hostPart, port = authority[:k], authority[k+1:]
	}
	host, err := normalizeURLHost(hostPart)
	if err != nil {
		return "", err
	}
	if port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 0 || n > 65535 || strings.ContainsAny(port, "+-") {
			return "", &urlError{"url_parsing", "Input should be a valid URL, invalid port number"}
		}
		port = strconv.Itoa(n)
		if port == defPort {
			port = ""
		}
	}
	path, query, fragment := tail, "", ""
	hasQuery, hasFragment := false, false
	if k := strings.IndexByte(path, '#'); k >= 0 {
		path, fragment, hasFragment = path[:k], path[k+1:], true
	}
	if k := strings.IndexByte(path, '?'); k >= 0 {
		path, query, hasQuery = path[:k], path[k+1:], true
	}
	var b strings.Builder
	b.WriteString(scheme + "://")
	if userinfo != "" {
		user, pass, hasPass := strings.Cut(userinfo, ":")
		b.WriteString(percentEncodeSet(user, userinfoEncodeSet))
		if hasPass && pass != "" {
			b.WriteString(":" + percentEncodeSet(pass, userinfoEncodeSet))
		}
		b.WriteString("@")
	}
	b.WriteString(host)
	if port != "" {
		b.WriteString(":" + port)
	}
	b.WriteString(normalizeURLPath(path))
	if hasQuery {
		b.WriteString("?" + percentEncodeSet(query, querySpecialEncodeSet))
	}
	if hasFragment {
		b.WriteString("#" + percentEncodeSet(fragment, fragmentEncodeSet))
	}
	return b.String(), nil
}

const (
	querySpecialEncodeSet = " \"#<>'"
	fragmentEncodeSet     = " \"<>`"
	pathEncodeSet         = " \"#<>?`{}"
	userinfoEncodeSet     = " \"#<>?`{}/:;=@[\\]^|"
)

// Percent encodes C0 controls, non ASCII bytes and the listed characters, leaving escapes intact
func percentEncodeSet(s, set string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x7f || strings.IndexByte(set, c) >= 0 {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Resolves dot segments and percent encodes a special URL path, keeping an empty path empty
func normalizeURLPath(path string) string {
	if path == "" {
		return ""
	}
	segments := strings.Split(path[1:], "/")
	var out []string
	for idx, seg := range segments {
		lower := strings.ToLower(seg)
		last := idx == len(segments)-1
		switch lower {
		case "..", ".%2e", "%2e.", "%2e%2e":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			if last {
				out = append(out, "")
			}
		case ".", "%2e":
			if last {
				out = append(out, "")
			}
		default:
			out = append(out, percentEncodeSet(seg, pathEncodeSet))
		}
	}
	return "/" + strings.Join(out, "/")
}

// Lowercases, maps through IDNA and validates a URL host, rendering IP literals canonically
func normalizeURLHost(host string) (string, error) {
	if host == "" {
		return "", &urlError{"url_parsing", "Input should be a valid URL, empty host"}
	}
	if strings.HasPrefix(host, "[") {
		addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
		if err != nil || !addr.Is6() {
			return "", &urlError{"url_parsing", "Input should be a valid URL, invalid IPv6 address"}
		}
		return "[" + addr.String() + "]", nil
	}
	decoded := pyUnquote(host)
	ascii := decoded
	needsIDNA := false
	for _, r := range decoded {
		if r >= utf8.RuneSelf || unicode.IsUpper(r) {
			needsIDNA = true
			break
		}
	}
	if needsIDNA {
		mapped, err := idnaProfile.ToASCII(decoded)
		if err != nil {
			return "", &urlError{"url_parsing", "Input should be a valid URL, invalid international domain name"}
		}
		ascii = mapped
	}
	ascii = strings.ToLower(ascii)
	if strings.ContainsAny(ascii, " #%/:<>?@[\\]^|\x00\t\n\r") {
		return "", &urlError{"url_parsing", "Input should be a valid URL, invalid domain character"}
	}
	if v4, ok, err := parseWHATWGIPv4(ascii); err != nil {
		return "", &urlError{"url_parsing", "Input should be a valid URL, invalid IPv4 address"}
	} else if ok {
		return v4, nil
	}
	return ascii, nil
}

var idnaProfile = idna.New(idna.MapForLookup(), idna.Transitional(false), idna.CheckHyphens(false), idna.StrictDomainName(false), idna.VerifyDNSLength(false))

// Parses a WHATWG IPv4 host with hex, octal and shortened forms, ok false when the host is a domain
func parseWHATWGIPv4(host string) (string, bool, error) {
	parts := strings.Split(host, ".")
	if parts[len(parts)-1] == "" && len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	last := parts[len(parts)-1]
	if _, ok := whatwgIPv4Number(last); !ok {
		allDigits := last != ""
		for i := 0; i < len(last); i++ {
			if last[i] < '0' || last[i] > '9' {
				allDigits = false
			}
		}
		if !allDigits {
			return "", false, nil
		}
	}
	if len(parts) > 4 {
		return "", false, errors.New("too many parts")
	}
	nums := make([]uint64, 0, len(parts))
	for _, p := range parts {
		n, ok := whatwgIPv4Number(p)
		if !ok {
			return "", false, errors.New("bad part")
		}
		nums = append(nums, n)
	}
	for _, n := range nums[:len(nums)-1] {
		if n > 255 {
			return "", false, errors.New("part overflow")
		}
	}
	lastN := nums[len(nums)-1]
	if lastN >= 1<<(8*(5-len(nums))) {
		return "", false, errors.New("overflow")
	}
	ipv4 := lastN
	for i, n := range nums[:len(nums)-1] {
		ipv4 += n << (8 * (3 - i))
	}
	return fmt.Sprintf("%d.%d.%d.%d", ipv4>>24&255, ipv4>>16&255, ipv4>>8&255, ipv4&255), true, nil
}

// Value of one WHATWG IPv4 part in decimal, hex or octal notation
func whatwgIPv4Number(p string) (uint64, bool) {
	if p == "" {
		return 0, false
	}
	base := 10
	if len(p) >= 2 && (p[:2] == "0x" || p[:2] == "0X") {
		p, base = p[2:], 16
	} else if len(p) >= 2 && p[0] == '0' {
		p, base = p[1:], 8
	}
	if p == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(p, base, 64)
	return n, err == nil
}

// Python-style whitespace split, the separators str.split uses without arguments
func pyWhitespaceFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
	})
}

// First n code points of a string the way a Python slice cuts it
func pyHead(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
