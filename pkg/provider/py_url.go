package provider

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

type pyURLParts struct {
	scheme, netloc, path, params, query, fragment string
}

var (
	pyUsesNetloc = setOf("", "ftp", "http", "gopher", "nntp", "telnet", "imap", "wais", "file", "mms", "https", "shttp",
		"snews", "prospero", "rtsp", "rtsps", "rtspu", "rsync", "svn", "svn+ssh", "sftp", "nfs", "git", "git+ssh", "ws", "wss", "itms-services")
	pyUsesParams = setOf("", "ftp", "hdl", "prospero", "http", "imap", "https", "shttp", "rtsp", "rtsps", "rtspu", "sip",
		"sips", "mms", "sftp", "tel")
	pyIPvFutureRe = regexp.MustCompile(`^v[a-fA-F0-9]+\..+$`)
)

const pySchemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

// Python urlparse into scheme, netloc, path, params, query and fragment with its ValueError checks
func pyURLParse(raw, defaultScheme string) (pyURLParts, error) {
	u := strings.TrimLeftFunc(raw, func(r rune) bool { return r <= 0x20 })
	scheme := strings.TrimFunc(defaultScheme, func(r rune) bool { return r <= 0x20 })
	for _, unsafe := range []string{"\t", "\r", "\n"} {
		u, scheme = strings.ReplaceAll(u, unsafe, ""), strings.ReplaceAll(scheme, unsafe, "")
	}
	var p pyURLParts
	if i := strings.IndexByte(u, ':'); i > 0 && u[0] < 0x80 && ((u[0] >= 'a' && u[0] <= 'z') || (u[0] >= 'A' && u[0] <= 'Z')) &&
		strings.Trim(u[:i], pySchemeChars) == "" {
		scheme, u = strings.ToLower(u[:i]), u[i+1:]
	}
	p.scheme = scheme
	if strings.HasPrefix(u, "//") {
		end := len(u)
		for _, c := range "/?#" {
			if w := strings.IndexRune(u[2:], c); w >= 0 && w+2 < end {
				end = w + 2
			}
		}
		p.netloc, u = u[2:end], u[end:]
		open, closed := strings.Contains(p.netloc, "["), strings.Contains(p.netloc, "]")
		if open != closed {
			return p, errors.New("Invalid IPv6 URL")
		}
		if open {
			if err := pyCheckBracketedNetloc(p.netloc); err != nil {
				return p, err
			}
		}
	}
	if before, after, found := strings.Cut(u, "#"); found {
		u, p.fragment = before, after
	}
	if before, after, found := strings.Cut(u, "?"); found {
		u, p.query = before, after
	}
	if err := pyCheckNetloc(p.netloc); err != nil {
		return p, err
	}
	p.path = u
	if pyUsesParams[p.scheme] && strings.Contains(u, ";") {
		start := strings.LastIndexByte(u, '/')
		if start < 0 {
			start = 0
		}
		if i := strings.IndexByte(u[start:], ';'); i >= 0 {
			p.path, p.params = u[:start+i], u[start+i+1:]
		}
	}
	return p, nil
}

// Bracketed host must be an IPv6 address or a well formed IPvFuture literal
func pyCheckBracketedNetloc(netloc string) error {
	hostPort := netloc[strings.LastIndexByte(netloc, '@')+1:]
	before, bracketed, open := strings.Cut(hostPort, "[")
	var host string
	if open {
		if before != "" {
			return errors.New("Invalid IPv6 URL")
		}
		var port string
		host, port, _ = strings.Cut(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			return errors.New("Invalid IPv6 URL")
		}
	} else {
		host, _, _ = strings.Cut(hostPort, ":")
	}
	if strings.HasPrefix(host, "v") {
		if !pyIPvFutureRe.MatchString(host) {
			return errors.New("IPvFuture address is invalid")
		}
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%s does not appear to be an IPv4 or IPv6 address", pyRepr(host))
	}
	if addr.Is4() {
		return errors.New("An IPv4 address cannot be in brackets")
	}
	return nil
}

// Rejects a non ASCII netloc whose NFKC form smuggles in a URL delimiter
func pyCheckNetloc(netloc string) error {
	ascii := true
	for i := 0; i < len(netloc); i++ {
		if netloc[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return nil
	}
	n := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
	normalized := norm.NFKC.String(n)
	if n == normalized {
		return nil
	}
	for _, c := range "/?#@:" {
		if strings.ContainsRune(normalized, c) {
			return fmt.Errorf("netloc '%s' contains invalid characters under NFKC normalization", netloc)
		}
	}
	return nil
}

// Python urlunparse of the six components
func pyURLUnparse(p pyURLParts) string {
	u := p.path
	if p.params != "" {
		u += ";" + p.params
	}
	switch {
	case p.netloc != "":
		if u != "" && !strings.HasPrefix(u, "/") {
			u = "/" + u
		}
		u = "//" + p.netloc + u
	case strings.HasPrefix(u, "//"):
		u = "//" + u
	case p.scheme != "" && pyUsesNetloc[p.scheme] && (u == "" || strings.HasPrefix(u, "/")):
		u = "//" + u
	}
	if p.scheme != "" {
		u = p.scheme + ":" + u
	}
	if p.query != "" {
		u += "?" + p.query
	}
	if p.fragment != "" {
		u += "#" + p.fragment
	}
	return u
}

// Percent decoding with invalid UTF-8 in the decoded bytes replaced, each run of invalid bytes one replacement character
func pyUnquoteReplace(s string) string {
	var out, pending strings.Builder
	flush := func() {
		out.WriteString(strings.ToValidUTF8(pending.String(), string(utf8.RuneError)))
		pending.Reset()
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHexByte(s[i+1]) && isHexByte(s[i+2]) {
			v, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			pending.WriteByte(byte(v))
			i += 2
			continue
		}
		flush()
		out.WriteByte(s[i])
	}
	flush()
	return out.String()
}
func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// Percent encodes every byte outside letters, digits and underscore dot dash tilde
func pyQuoteAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
