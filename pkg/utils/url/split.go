package urlutil

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Components of a URL reference cut at its delimiters, nothing decoded, the scheme lowercased and the authority holding any user info and port
type Parts struct {
	Scheme, Authority, Path, Query, Fragment string
}

// Reference cut into scheme, authority, path, query and fragment the way RFC 3986 appendix B reads any text, after dropping leading C0 controls and spaces and every tab, CR and LF; an ErrInvalidHost error only for bad IP literal brackets or a non-ASCII authority that NFKC normalization gives URL delimiters
func Split(ref string) (Parts, error) {
	ref = cleanRef(ref)
	var p Parts
	ref, p.Fragment, _ = strings.Cut(ref, "#")
	ref, p.Query, _ = strings.Cut(ref, "?")
	if i := schemeEnd(ref); i > 0 {
		p.Scheme, ref = strings.ToLower(ref[:i]), ref[i+1:]
	}
	if rest, ok := strings.CutPrefix(ref, "//"); ok {
		end := strings.IndexByte(rest, '/')
		if end < 0 {
			end = len(rest)
		}
		p.Authority, ref = rest[:end], rest[end:]
	}
	p.Path = ref
	if _, _, err := hostPort(p.Authority); err != nil {
		return Parts{}, err
	}
	if err := checkNormalized(p.Authority); err != nil {
		return Parts{}, err
	}
	return p, nil
}

// Reference without its leading C0 controls and spaces and without any tab, CR or LF, the text browsers go on to read
func cleanRef(ref string) string {
	ref = strings.TrimLeftFunc(ref, func(r rune) bool { return r <= ' ' })
	if strings.ContainsAny(ref, "\t\r\n") {
		ref = strings.Map(func(r rune) rune {
			if r == '\t' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, ref)
	}
	return ref
}

// Index of the colon ending a leading scheme, a letter then letters, digits, + - or ., and -1 without one
func schemeEnd(s string) int {
	for i := range len(s) {
		switch c := s[i]; {
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case '0' <= c && c <= '9' || c == '+' || c == '-' || c == '.':
			if i == 0 {
				return -1
			}
		case c == ':':
			if i == 0 {
				return -1
			}
			return i
		default:
			return -1
		}
	}
	return -1
}

// Refuses a non-ASCII authority whose NFKC form gains a / ? # @ or : it did not have, text another client would split differently
func checkNormalized(authority string) error {
	ascii := true
	for i := range len(authority) {
		if authority[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return nil
	}
	stripped := strings.Map(func(r rune) rune {
		switch r {
		case '@', ':', '#', '?':
			return -1
		}
		return r
	}, authority)
	if normalized := norm.NFKC.String(stripped); normalized != stripped && strings.ContainsAny(normalized, "/?#@:") {
		return fmt.Errorf("%w: %q gains URL delimiters under NFKC normalization", ErrInvalidHost, authority)
	}
	return nil
}

// Reference put back together from its components, // written before a non-empty authority or a path that starts with //, an empty query or fragment left out
func (p Parts) String() string {
	s := p.Path
	if p.Authority != "" || strings.HasPrefix(s, "//") {
		if s != "" && s[0] != '/' {
			s = "/" + s
		}
		s = "//" + p.Authority + s
	}
	if p.Scheme != "" {
		s = p.Scheme + ":" + s
	}
	if p.Query != "" {
		s += "?" + p.Query
	}
	if p.Fragment != "" {
		s += "#" + p.Fragment
	}
	return s
}

// Host of the authority, user info and port dropped, IP literal brackets removed, lowercased with an IPv6 zone kept as written
func (p Parts) Hostname() string {
	host, _, _ := hostPort(p.Authority)
	return lowerHost(host)
}

// Port number of the authority, false when it names none, an error when it is not a decimal number, one wrapping ErrPortRange past 65535
func (p Parts) Port() (int, bool, error) {
	_, text, err := hostPort(p.Authority)
	if err != nil {
		return 0, false, err
	}
	if text == "" {
		return 0, false, nil
	}
	n, err := parsePort(text)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}
