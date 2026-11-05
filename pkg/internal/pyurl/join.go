package pyurl

import "strings"

var usesNetloc = map[string]bool{"": true, "ftp": true, "http": true, "gopher": true, "nntp": true, "telnet": true, "imap": true,
	"wais": true, "file": true, "mms": true, "https": true, "shttp": true, "snews": true, "prospero": true, "rtsp": true,
	"rtsps": true, "rtspu": true, "rsync": true, "svn": true, "svn+ssh": true, "sftp": true, "nfs": true, "git": true,
	"git+ssh": true, "ws": true, "wss": true, "itms-services": true}

// Split parts with absent netloc, query and fragment told apart from empty ones like the 3.14 _urlsplit
type optParts struct {
	scheme                                 string
	hasScheme, hasNetloc, hasQuery, hasFrg bool
	u                                      URL
}

func splitOpt(raw string) (optParts, error) {
	u, err := Split(raw)
	if err != nil {
		return optParts{}, err
	}
	p := optParts{u: u, scheme: u.Scheme, hasScheme: u.Scheme != ""}
	rest := strings.TrimLeft(raw, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	rest = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(rest)
	if p.hasScheme {
		rest = rest[strings.IndexByte(rest, ':')+1:]
	}
	if strings.HasPrefix(rest, "//") {
		p.hasNetloc = true
		rest = rest[2+len(u.Netloc):]
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		p.hasFrg, rest = true, rest[:i]
	}
	p.hasQuery = strings.Contains(rest, "?")
	return p, nil
}

func (p optParts) unsplit() string {
	url := p.u.Path
	if p.hasNetloc {
		if url != "" && url[0] != '/' {
			url = "/" + url
		}
		url = "//" + p.u.Netloc + url
	} else if strings.HasPrefix(url, "//") {
		url = "//" + url
	}
	if p.scheme != "" {
		url = p.scheme + ":" + url
	}
	if p.hasQuery {
		url += "?" + p.u.Query
	}
	if p.hasFrg {
		url += "#" + p.u.Fragment
	}
	return url
}

// Resolves a reference against a base URL like urllib.parse.urljoin of CPython 3.14
func Join(base, ref string) (string, error) {
	if base == "" {
		return ref, nil
	}
	if ref == "" {
		return base, nil
	}
	b, err := splitOpt(base)
	if err != nil {
		return "", err
	}
	r, err := splitOpt(ref)
	if err != nil {
		return "", err
	}
	if !r.hasScheme {
		r.scheme = b.scheme
	}
	if r.scheme != b.scheme || (r.scheme != "" && !UsesRelative[r.scheme]) {
		return ref, nil
	}
	if r.scheme == "" || usesNetloc[r.scheme] {
		if r.hasNetloc && r.u.Netloc != "" {
			return r.unsplit(), nil
		}
		r.hasNetloc, r.u.Netloc = b.hasNetloc, b.u.Netloc
	}
	if r.u.Path == "" {
		r.u.Path = b.u.Path
		if !r.hasQuery {
			r.hasQuery, r.u.Query = b.hasQuery, b.u.Query
			if !r.hasFrg {
				r.hasFrg, r.u.Fragment = b.hasFrg, b.u.Fragment
			}
		}
		return r.unsplit(), nil
	}
	baseParts := strings.Split(b.u.Path, "/")
	if baseParts[len(baseParts)-1] != "" {
		baseParts = baseParts[:len(baseParts)-1]
	}
	var segments []string
	if strings.HasPrefix(r.u.Path, "/") {
		segments = strings.Split(r.u.Path, "/")
	} else {
		segments = append(baseParts, strings.Split(r.u.Path, "/")...)
		if len(segments) > 2 {
			kept := []string{segments[0]}
			for _, s := range segments[1 : len(segments)-1] {
				if s != "" {
					kept = append(kept, s)
				}
			}
			segments = append(kept, segments[len(segments)-1])
		}
	}
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
	r.u.Path = strings.Join(resolved, "/")
	if r.u.Path == "" {
		r.u.Path = "/"
	}
	return r.unsplit(), nil
}
