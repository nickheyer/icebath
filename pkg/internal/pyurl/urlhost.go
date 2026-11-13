package pyurl

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Host in HTTP URL authority form, bracketing IPv6 literals and escaping a zone percent
func FormatURLHost(host string) string {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") && len(host) >= 2 {
		inner := host[1 : len(host)-1]
		if strings.Contains(inner, ":") {
			return "[" + strings.ReplaceAll(inner, "%", "%25") + "]"
		}
		return host
	}
	if strings.Contains(host, ":") {
		return "[" + strings.ReplaceAll(host, "%", "%25") + "]"
	}
	return host
}

// Scheme to proxy map read from the environment the way urllib getproxies_environment reads it
func EnvironProxies(environ []string) map[string]string {
	proxies := map[string]string{}
	pairs := make([][2]string, 0, len(environ))
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		pairs = append(pairs, [2]string{name, value})
	}
	for _, p := range pairs {
		name := strings.ToLower(p[0])
		if p[1] != "" && strings.HasSuffix(name, "_proxy") {
			proxies[name[:len(name)-6]] = p[1]
		}
	}
	for _, p := range pairs {
		if p[0] == "REQUEST_METHOD" {
			delete(proxies, "http")
			break
		}
	}
	for _, p := range pairs {
		if strings.HasSuffix(p[0], "_proxy") {
			name := strings.ToLower(p[0])
			if p[1] != "" {
				proxies[name[:len(name)-6]] = p[1]
			} else {
				delete(proxies, name[:len(name)-6])
			}
		}
	}
	return proxies
}

// Reports whether no_proxy exempts the host the way urllib proxy_bypass_environment decides
func ProxyBypassEnvironment(host string, proxies map[string]string) bool {
	noProxy, ok := proxies["no"]
	if !ok {
		return false
	}
	if noProxy == "*" {
		return true
	}
	host = strings.ToLower(host)
	hostonly := host
	if i := strings.LastIndex(host, ":"); i >= 0 && strings.Trim(host[i+1:], "0123456789") == "" {
		hostonly = host[:i]
	}
	for _, name := range strings.Split(noProxy, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		name = strings.ToLower(strings.TrimLeft(name, "."))
		if hostonly == name || host == name {
			return true
		}
		name = "." + name
		if strings.HasSuffix(hostonly, name) || strings.HasSuffix(host, name) {
			return true
		}
	}
	return false
}

// Transport proxy choice of urllib urlopen on posix, environment proxies without a loopback exemption
func UrlopenProxy(req *http.Request) (*url.URL, error) {
	return EnvironUrlopenProxy(os.Environ(), req)
}

// Urlopen proxy choice over an explicit environment in place of the process one
func EnvironUrlopenProxy(environ []string, req *http.Request) (*url.URL, error) {
	proxies := EnvironProxies(environ)
	proxy, ok := proxies[req.URL.Scheme]
	if !ok || req.URL.Host == "" || ProxyBypassEnvironment(req.URL.Host, proxies) {
		return nil, nil
	}
	if !strings.Contains(proxy, "://") {
		proxy = req.URL.Scheme + "://" + proxy
	}
	return url.Parse(proxy)
}
