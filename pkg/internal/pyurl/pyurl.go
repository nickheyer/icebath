package pyurl

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// URL holds the parts urllib.parse splits a URL into, params apart from the path
type URL struct {
	Scheme, Netloc, Path, Params, Query, Fragment string
}

// ErrInvalidIPv6 is the ValueError urlsplit raises for unbalanced or malformed brackets
var ErrInvalidIPv6 = errors.New("Invalid IPv6 URL")

func isASCIIAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// Splits like urllib.parse.urlsplit including its control stripping and bracket checks
func Split(raw string) (URL, error) {
	var u URL
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
			return URL{}, ErrInvalidIPv6
		}
		if open && closed {
			if err := checkBracketedNetloc(u.Netloc); err != nil {
				return URL{}, err
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
	if err := checkNetloc(u.Netloc); err != nil {
		return URL{}, err
	}
	return u, nil
}

// Rejects a non ASCII netloc whose NFKC form gains URL delimiters, the urlsplit netloc check
func checkNetloc(netloc string) error {
	ascii := true
	for i := 0; i < len(netloc); i++ {
		ascii = ascii && netloc[i] < utf8.RuneSelf
	}
	if ascii {
		return nil
	}
	n := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
	normalized := norm.NFKC.String(n)
	if n != normalized && strings.ContainsAny(normalized, "/?#@:") {
		return fmt.Errorf("netloc '%s' contains invalid characters under NFKC normalization", netloc)
	}
	return nil
}

// Validates the bracketed host of a netloc the way urlsplit checks it
func checkBracketedNetloc(netloc string) error {
	hostinfo := netloc[strings.LastIndex(netloc, "@")+1:]
	open := strings.Index(hostinfo, "[")
	closeIdx := strings.Index(hostinfo, "]")
	if open < 0 || closeIdx < open {
		return ErrInvalidIPv6
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

var UsesParams = map[string]bool{"": true, "ftp": true, "hdl": true, "prospero": true, "http": true, "imap": true, "https": true,
	"shttp": true, "rtsp": true, "rtsps": true, "rtspu": true, "sip": true, "sips": true, "mms": true, "sftp": true, "tel": true}

var UsesRelative = map[string]bool{"": true, "ftp": true, "http": true, "gopher": true, "nntp": true, "imap": true, "wais": true,
	"file": true, "https": true, "shttp": true, "mms": true, "prospero": true, "rtsp": true, "rtsps": true, "rtspu": true,
	"sftp": true, "svn": true, "svn+ssh": true, "ws": true, "wss": true}

// Parses like urllib.parse.urlparse, moving the last segment's params out of the path
func Parse(raw string) (URL, error) {
	u, err := Split(raw)
	if err != nil {
		return u, err
	}
	if UsesParams[u.Scheme] && strings.Contains(u.Path, ";") {
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
