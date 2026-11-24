package urlutil

import "strings"

// Parts of a configured base URL that may lack a scheme, a bare host, host and port or host and path read as the authority, false when blank; only a scheme at the very start followed by // counts, so a :// further on (a query holding a URL) does not, and a reference already starting with // keeps its authority
func parseBase(baseURL string) (Parts, bool, error) {
	raw := cleanRef(strings.TrimSpace(baseURL))
	if raw == "" {
		return Parts{}, false, nil
	}
	if i := schemeEnd(raw); !strings.HasPrefix(raw, "//") && (i <= 0 || !strings.HasPrefix(raw[i:], "://")) {
		raw = "//" + raw
	}
	p, err := Split(raw)
	return p, err == nil, err
}

// Lowercased host of a base URL that may lack a scheme, trailing dots removed, empty when blank or hostless, an ErrInvalidHost error for an authority Split refuses
func BaseHost(baseURL string) (string, error) {
	p, ok, err := parseBase(baseURL)
	if !ok {
		return "", err
	}
	return strings.TrimRight(p.Hostname(), "."), nil
}

// Whether the host of a base URL is the domain or one of its subdomains, compared without case or trailing dots
func BaseHostMatches(baseURL, domain string) (bool, error) {
	host, err := BaseHost(baseURL)
	if err != nil {
		return false, err
	}
	domain = strings.TrimRight(strings.ToLower(strings.TrimSpace(domain)), ".")
	return host != "" && domain != "" && (host == domain || strings.HasSuffix(host, "."+domain)), nil
}

// Lowercased scheme, host and effective port of a base URL, 443 or 80 standing in for an absent http(s) port, all empty with port 0 when hostless or the port is not a number up to 65535
func BaseOrigin(baseURL string) (scheme, host string, port int, err error) {
	p, ok, err := parseBase(baseURL)
	if !ok {
		return "", "", 0, err
	}
	if host = strings.TrimRight(p.Hostname(), "."); host == "" {
		return "", "", 0, nil
	}
	port, present, perr := p.Port()
	switch {
	case perr != nil:
		return "", "", 0, nil
	case present:
		return p.Scheme, host, port, nil
	case p.Scheme == "https":
		return p.Scheme, host, 443, nil
	case p.Scheme == "http":
		return p.Scheme, host, 80, nil
	}
	return p.Scheme, host, 0, nil
}

// Lowercased path of a base URL without trailing slashes, empty for a bare host, an ErrInvalidHost error for an authority Split refuses
func BasePath(baseURL string) (string, error) {
	p, ok, err := parseBase(baseURL)
	if !ok {
		return "", err
	}
	return strings.TrimRight(strings.ToLower(p.Path), "/"), nil
}
