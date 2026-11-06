package pyurl

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Reassembles split parts like urllib.parse.urlunsplit, empty query and fragment dropped
func Unsplit(u URL) string {
	p := optParts{u: u, scheme: u.Scheme, hasQuery: u.Query != "", hasFrg: u.Fragment != ""}
	p.hasNetloc = u.Netloc != "" || (u.Scheme != "" && usesNetloc[u.Scheme] && (u.Path == "" || u.Path[0] == '/'))
	return p.unsplit()
}

// Reassembles parsed parts like urllib.parse.urlunparse, params rejoined to the path
func Unparse(u URL) string {
	if u.Params != "" {
		u.Path += ";" + u.Params
	}
	u.Params = ""
	return Unsplit(u)
}

// Hostname of the netloc like SplitResult.hostname, lowercased with any zone kept, empty when absent
func (u URL) Hostname() string {
	hostinfo := u.Netloc[strings.LastIndex(u.Netloc, "@")+1:]
	var host string
	if _, bracketed, open := strings.Cut(hostinfo, "["); open {
		host, _, _ = strings.Cut(bracketed, "]")
	} else {
		host, _, _ = strings.Cut(hostinfo, ":")
	}
	if host == "" {
		return ""
	}
	h, zone, hasZone := strings.Cut(host, "%")
	if hasZone {
		return pystr.Lower(h) + "%" + zone
	}
	return pystr.Lower(h)
}

// Lowercased hostname of a base URL that may lack a scheme, the urlparse ValueError on a bad netloc
func BaseURLHostname(baseURL string) (string, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := Parse(raw)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(strings.ToLower(u.Hostname()), "."), nil
}

// Reports whether the base URL hostname is the domain or one of its subdomains
func BaseURLHostMatches(baseURL, domain string) (bool, error) {
	host, err := BaseURLHostname(baseURL)
	if err != nil {
		return false, err
	}
	domain = strings.TrimRight(strings.ToLower(strings.TrimSpace(domain)), ".")
	return host != "" && domain != "" && (host == domain || strings.HasSuffix(host, "."+domain)), nil
}
