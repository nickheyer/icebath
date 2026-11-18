package pyhttpx

import (
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// URLPattern of one httpx mount with the proxy it routes to, nil for a direct mount
type proxyMount struct {
	scheme string
	host   string
	hostRE *regexp.Regexp
	port   int
	proxy  *url.URL
}

// Proxy mount table an httpx client built with trust_env reads at construction, most specific first
type EnvProxies struct{ mounts []proxyMount }

// Mounts get_environment_proxies builds from a getproxies map, an unknown proxy scheme failing as httpx Proxy fails
func NewEnvProxies(proxies map[string]string) (*EnvProxies, error) {
	var keys []string
	targets := map[string]string{}
	put := func(key, proxy string) {
		if _, seen := targets[key]; !seen {
			keys = append(keys, key)
		}
		targets[key] = proxy
	}
	for _, scheme := range []string{"http", "https", "all"} {
		if value := proxies[scheme]; value != "" {
			if !strings.Contains(value, "://") {
				value = "http://" + value
			}
			put(scheme+"://", value)
		}
	}
	for _, raw := range strings.Split(proxies["no"], ",") {
		hostname := strings.TrimSpace(raw)
		switch {
		case hostname == "*":
			return &EnvProxies{}, nil
		case hostname == "":
		case strings.Contains(hostname, "://"):
			put(hostname, "")
		case isIPHostname(hostname, false):
			put("all://"+hostname, "")
		case isIPHostname(hostname, true):
			put("all://["+hostname+"]", "")
		case strings.ToLower(hostname) == "localhost":
			put("all://"+hostname, "")
		default:
			put("all://*"+hostname, "")
		}
	}
	out := &EnvProxies{}
	for _, key := range keys {
		m := parseURLPattern(key)
		if target := targets[key]; target != "" {
			proxyURL, err := url.Parse(target)
			if err != nil {
				return nil, err
			}
			switch strings.ToLower(proxyURL.Scheme) {
			case "http", "https", "socks5", "socks5h":
			default:
				return nil, unknownProxyScheme(proxyURL)
			}
			m.proxy = proxyURL
		}
		out.mounts = append(out.mounts, m)
	}
	sort.SliceStable(out.mounts, func(i, j int) bool { return out.mounts[i].less(out.mounts[j]) })
	return out, nil
}

// Proxy the client sends a request for u through, nil for a direct connection
func (p *EnvProxies) For(u *url.URL) *url.URL {
	if p == nil || u == nil {
		return nil
	}
	for _, m := range p.mounts {
		if m.matches(u) {
			return m.proxy
		}
	}
	return nil
}

// IPv4 or IPv6 address before any slash, the ipaddress test httpx applies to a NO_PROXY entry
func isIPHostname(hostname string, v6 bool) bool {
	addr, err := netip.ParseAddr(strings.SplitN(hostname, "/", 2)[0])
	if err != nil {
		return false
	}
	if v6 {
		return addr.Is6()
	}
	return addr.Is4()
}

// Port httpx URL keeps, none for the default port of http and https
func normalizedPort(scheme, port string) int {
	n, err := strconv.Atoi(port)
	if err != nil || (n == 80 && scheme == "http") || (n == 443 && scheme == "https") {
		return -1
	}
	return n
}

// URLPattern of a mount key, all standing for any scheme and a star host for any host
func parseURLPattern(key string) proxyMount {
	scheme, rest, _ := strings.Cut(key, "://")
	scheme = strings.ToLower(scheme)
	authority := rest
	if at := strings.IndexAny(authority, "/?#"); at >= 0 {
		authority = authority[:at]
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	host, port := authority, ""
	if strings.HasPrefix(authority, "[") {
		if end := strings.Index(authority, "]"); end >= 0 {
			host = authority[1:end]
			port = strings.TrimPrefix(authority[end+1:], ":")
		}
	} else if at := strings.LastIndex(authority, ":"); at >= 0 {
		host, port = authority[:at], authority[at+1:]
	}
	host = strings.ToLower(host)
	m := proxyMount{scheme: scheme, host: host, port: normalizedPort(scheme, port)}
	if scheme == "all" {
		m.scheme = ""
	}
	switch {
	case host == "" || host == "*":
		m.host = ""
	case strings.HasPrefix(host, "*."):
		m.hostRE = regexp.MustCompile(`^.+\.` + regexp.QuoteMeta(host[2:]) + `$`)
	case strings.HasPrefix(host, "*"):
		m.hostRE = regexp.MustCompile(`^(.+\.)?` + regexp.QuoteMeta(host[1:]) + `$`)
	default:
		m.hostRE = regexp.MustCompile(`^` + regexp.QuoteMeta(host) + `$`)
	}
	return m
}

// Scheme, host and port test of URLPattern.matches
func (m proxyMount) matches(u *url.URL) bool {
	scheme := strings.ToLower(u.Scheme)
	if m.scheme != "" && m.scheme != scheme {
		return false
	}
	if m.host != "" && m.hostRE != nil && !m.hostRE.MatchString(strings.ToLower(u.Hostname())) {
		return false
	}
	return m.port < 0 || m.port == normalizedPort(scheme, u.Port())
}

// Priority order of URLPattern, a port first, then the longer host, then the longer scheme
func (m proxyMount) less(o proxyMount) bool {
	a := [3]int{portPriority(m.port), -len(m.host), -len(m.scheme)}
	b := [3]int{portPriority(o.port), -len(o.host), -len(o.scheme)}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Zero for a pattern with a port, one for one without
func portPriority(port int) int {
	if port >= 0 {
		return 0
	}
	return 1
}
