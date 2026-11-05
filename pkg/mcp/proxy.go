package mcp

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
)

var splitPortRE = regexp.MustCompile(`(?s)^(.*):([0-9]*)$`)

// Operating system proxy sources, replaced in tests so the host configuration cannot leak in
var (
	systemProxies     = getproxiesSystem
	systemProxyBypass = proxyBypassSystem
)

// pySplitPort splits host and a numeric port the way urllib _splitport does, empty port when absent
func pySplitPort(host string) (string, string) {
	if m := splitPortRE.FindStringSubmatch(host); m != nil {
		return m[1], m[2]
	}
	return host, ""
}

// getproxiesEnvironment reads every NAME_proxy variable with lowercase names winning and CGI dropping http
func getproxiesEnvironment() map[string]string {
	type entry struct{ name, value, proxy string }
	proxies := map[string]string{}
	var environment []entry
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if len(name) > 5 && name[len(name)-6] == '_' && strings.ToLower(name[len(name)-5:]) == "proxy" {
			e := entry{name, value, strings.ToLower(name[:len(name)-6])}
			environment = append(environment, e)
			if value != "" {
				proxies[e.proxy] = value
			}
		}
	}
	if _, cgi := os.LookupEnv("REQUEST_METHOD"); cgi {
		delete(proxies, "http")
	}
	for _, e := range environment {
		if strings.HasSuffix(e.name, "_proxy") {
			if e.value != "" {
				proxies[e.proxy] = e.value
			} else {
				delete(proxies, e.proxy)
			}
		}
	}
	return proxies
}

// proxyBypassEnvironment matches host against the no_proxy suffix list, star bypassing every host
func proxyBypassEnvironment(host string, proxies map[string]string) bool {
	noProxy, ok := proxies["no"]
	if !ok {
		return false
	}
	if noProxy == "*" {
		return true
	}
	host = strings.ToLower(host)
	hostonly, _ := pySplitPort(host)
	for _, name := range strings.Split(noProxy, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		name = strings.ToLower(strings.TrimLeft(name, "."))
		if hostonly == name || host == name {
			return true
		}
		if strings.HasSuffix(hostonly, "."+name) || strings.HasSuffix(host, "."+name) {
			return true
		}
	}
	return false
}

// getproxies returns the environment proxies, or the operating system proxies when the environment sets none
func getproxies() map[string]string {
	if p := getproxiesEnvironment(); len(p) > 0 {
		return p
	}
	return systemProxies()
}

// proxyBypass consults the environment bypass list when any proxy variable is set, else the system list
func proxyBypass(host string) bool {
	if p := getproxiesEnvironment(); len(p) > 0 {
		return proxyBypassEnvironment(host, p)
	}
	return systemProxyBypass(host)
}

// normalizeProxyURL rewrites the socks scheme shells export to socks5 and blanks whitespace only values
func normalizeProxyURL(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(strings.ToLower(p), "socks://") {
		return "socks5://" + p[len("socks://"):]
	}
	return p
}

// mcpProxyMounts maps http and https to the proxy the server URL dials through, nil for a direct connect
func mcpProxyMounts(rawURL string) (map[string]*url.URL, error) {
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if host == "" || provider.IsProxyLoopbackHost(host) || provider.ShouldBypassProxy([]string{rawURL}, nil) || proxyBypass(host) {
		return nil, nil
	}
	proxies := getproxies()
	mounts := map[string]*url.URL{}
	for _, scheme := range []string{"http", "https"} {
		value := proxies[scheme]
		if value == "" {
			value = proxies["all"]
		}
		proxyURL := normalizeProxyURL(value)
		if proxyURL == "" {
			continue
		}
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("Unknown scheme for proxy URL URL('%s')", proxyURL)
		}
		mounts[scheme] = parsed
	}
	if len(mounts) == 0 {
		return nil, nil
	}
	return mounts, nil
}
