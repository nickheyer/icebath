package jsonschemautil

import (
	"bytes"
	"strings"
)

// URI reference cut at its delimiters as RFC 3986 appendix B reads it, nothing decoded, with which optional parts are present
type uriParts struct {
	scheme, authority, path, query, fragment string
	hasAuthority, hasQuery, hasFragment      bool
}

func parseURI(s string) uriParts {
	var p uriParts
	if i := strings.IndexByte(s, '#'); i >= 0 {
		p.fragment, p.hasFragment, s = s[i+1:], true, s[:i]
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		p.query, p.hasQuery, s = s[i+1:], true, s[:i]
	}
	if i := schemeEnd(s); i > 0 {
		p.scheme, s = strings.ToLower(s[:i]), s[i+1:]
	}
	if rest, ok := strings.CutPrefix(s, "//"); ok {
		end := strings.IndexByte(rest, '/')
		if end < 0 {
			end = len(rest)
		}
		p.authority, p.hasAuthority, s = rest[:end], true, rest[end:]
	}
	p.path = s
	return p
}

// Index of the colon ending a leading scheme, a letter followed by letters, digits, + - and ., or -1 when there is none
func schemeEnd(s string) int {
	for i := range len(s) {
		switch c := s[i]; {
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'):
		case i > 0 && c == ':':
			return i
		default:
			return -1
		}
	}
	return -1
}

func (p uriParts) String() string {
	var b strings.Builder
	if p.scheme != "" {
		b.WriteString(p.scheme)
		b.WriteByte(':')
	}
	if p.hasAuthority {
		b.WriteString("//")
		b.WriteString(p.authority)
	}
	b.WriteString(p.path)
	if p.hasQuery {
		b.WriteByte('?')
		b.WriteString(p.query)
	}
	if p.hasFragment {
		b.WriteByte('#')
		b.WriteString(p.fragment)
	}
	return b.String()
}

// Reference resolved against a base URI by RFC 3986 section 5.2, its fragment kept
func resolveURI(base, ref string) string {
	r := parseURI(ref)
	if r.scheme != "" {
		if strings.HasPrefix(r.path, "/") {
			r.path = removeDotSegments(r.path)
		}
		return r.String()
	}
	b := parseURI(base)
	t := uriParts{scheme: b.scheme, fragment: r.fragment, hasFragment: r.hasFragment}
	switch {
	case r.hasAuthority:
		t.authority, t.hasAuthority = r.authority, true
		t.path = removeDotSegments(r.path)
		t.query, t.hasQuery = r.query, r.hasQuery
	case r.path == "":
		t.authority, t.hasAuthority = b.authority, b.hasAuthority
		t.path = b.path
		t.query, t.hasQuery = b.query, b.hasQuery
		if r.hasQuery {
			t.query, t.hasQuery = r.query, true
		}
	default:
		t.authority, t.hasAuthority = b.authority, b.hasAuthority
		if strings.HasPrefix(r.path, "/") {
			t.path = removeDotSegments(r.path)
		} else {
			t.path = removeDotSegments(mergePath(b, r.path))
		}
		t.query, t.hasQuery = r.query, r.hasQuery
	}
	return t.String()
}

// Relative path appended to the directory of the base path, RFC 3986 section 5.2.3
func mergePath(b uriParts, ref string) string {
	if b.hasAuthority && b.path == "" {
		return "/" + ref
	}
	if i := strings.LastIndexByte(b.path, '/'); i >= 0 {
		return b.path[:i+1] + ref
	}
	return ref
}

// Path with its . and .. segments applied, RFC 3986 section 5.2.4
func removeDotSegments(path string) string {
	if !strings.Contains(path, ".") {
		return path
	}
	out := make([]byte, 0, len(path))
	drop := func() {
		if i := bytes.LastIndexByte(out, '/'); i >= 0 {
			out = out[:i]
		} else {
			out = out[:0]
		}
	}
	for in := path; in != ""; {
		switch {
		case strings.HasPrefix(in, "../"):
			in = in[3:]
		case strings.HasPrefix(in, "./"):
			in = in[2:]
		case strings.HasPrefix(in, "/./"):
			in = in[2:]
		case in == "/.":
			in = "/"
		case strings.HasPrefix(in, "/../"):
			in = in[3:]
			drop()
		case in == "/..":
			in = "/"
			drop()
		case in == "." || in == "..":
			in = ""
		default:
			start := 0
			if in[0] == '/' {
				start = 1
			}
			end := strings.IndexByte(in[start:], '/')
			if end < 0 {
				end = len(in)
			} else {
				end += start
			}
			out = append(out, in[:end]...)
			in = in[end:]
		}
	}
	return string(out)
}

// URI as the registry keys it: surrounding space and any fragment dropped, the scheme lowercased
func normalizeURI(uri string) string {
	p := parseURI(strings.TrimSpace(uri))
	p.fragment, p.hasFragment = "", false
	return p.String()
}

// URI without its fragment, and the fragment still percent-encoded
func splitFragment(uri string) (string, string) {
	base, frag, _ := strings.Cut(uri, "#")
	return base, frag
}

func isHex(c byte) bool { return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F' }

func isAlnum(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

func isUnreserved(c byte) bool { return isAlnum(c) || c == '-' || c == '.' || c == '_' || c == '~' }

func isSubDelim(c byte) bool { return strings.IndexByte("!$&'()*+,;=", c) >= 0 }
