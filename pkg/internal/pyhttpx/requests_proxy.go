package pyhttpx

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Proxy requests picks for a URL from the environment, nil when no_proxy or the urllib bypass exempts the host
func RequestsProxy(u *url.URL) (*url.URL, error) {
	if requestsBypassesProxies(u) {
		return nil, nil
	}
	proxies := pyurl.Getproxies()
	host := strings.ToLower(u.Hostname())
	var raw string
	found := false
	if host == "" {
		raw, found = proxies[u.Scheme]
		if !found {
			raw, found = proxies["all"]
		}
	} else {
		for _, key := range []string{u.Scheme + "://" + host, u.Scheme, "all://" + host, "all"} {
			if raw, found = proxies[key]; found {
				break
			}
		}
	}
	if !found || raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return url.Parse(raw)
}

// requests should_bypass_proxies, no_proxy entries matched as CIDR, IP, host or host with port, then urllib proxy_bypass
func requestsBypassesProxies(u *url.URL) bool {
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return true
	}
	noProxy := os.Getenv("no_proxy")
	if noProxy == "" {
		noProxy = os.Getenv("NO_PROXY")
	}
	if noProxy != "" {
		var hosts []string
		for _, h := range strings.Split(strings.ReplaceAll(noProxy, " ", ""), ",") {
			if h != "" {
				hosts = append(hosts, h)
			}
		}
		if ip := net.ParseIP(hostname); ip != nil && ip.To4() != nil && !strings.Contains(hostname, ":") {
			for _, entry := range hosts {
				if _, network, err := net.ParseCIDR(entry); err == nil && requestsValidCIDR(entry) {
					if network.Contains(ip) {
						return true
					}
				} else if hostname == entry {
					return true
				}
			}
		} else {
			hostWithPort := hostname
			if port := u.Port(); port != "" {
				if n, err := strconv.Atoi(port); err == nil && n != 0 {
					hostWithPort += ":" + port
				}
			}
			for _, entry := range hosts {
				entry = strings.TrimLeft(entry, ".")
				if hostname == entry || hostWithPort == entry {
					return true
				}
				entry = "." + entry
				if strings.HasSuffix(hostname, entry) || strings.HasSuffix(hostWithPort, entry) {
					return true
				}
			}
		}
	}
	return pyurl.ProxyBypassEnvironment(hostname, pyurl.EnvironProxies(os.Environ()))
}

// requests is_valid_cidr, a dotted IPv4 address with a mask between 1 and 32
func requestsValidCIDR(entry string) bool {
	addr, mask, ok := strings.Cut(entry, "/")
	if !ok || strings.Count(entry, "/") != 1 {
		return false
	}
	n, err := strconv.Atoi(mask)
	if err != nil || n < 1 || n > 32 {
		return false
	}
	ip := net.ParseIP(addr)
	return ip != nil && ip.To4() != nil && !strings.Contains(addr, ":")
}
