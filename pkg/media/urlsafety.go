package media

import (
	"net"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/tools"
)

var blockedHostnames = map[string]bool{
	"metadata.google.internal": true, "metadata": true, "instance-data": true,
	"169.254.169.254": true, "fd00:ec2::254": true, "100.100.100.200": true,
}

// Reports whether an address is a cloud metadata endpoint that stays blocked under every policy
func isAlwaysBlockedIP(ip net.IP) bool {
	for _, cidr := range []string{"169.254.169.254/32", "100.100.100.200/32", "fd00:ec2::254/128"} {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

// Unwraps IPv4 mapped IPv6 addresses to their IPv4 form
func embeddedIPv4(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

// Reports whether an address is private, loopback, link local, or otherwise internal
func isBlockedIP(ip net.IP) bool {
	ip = embeddedIPv4(ip)
	if isAlwaysBlockedIP(ip) {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		(ip.To4() != nil && ip[0] == 100 && ip[1]&0xc0 == 64)
}

// Lowercased hostname of a URL without a trailing dot
func urlHostname(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// Reports whether a URL targets a cloud metadata address by name or literal
func isAlwaysBlockedURL(rawURL string) bool {
	host := urlHostname(rawURL)
	if host == "" {
		return true
	}
	if blockedHostnames[host] {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isAlwaysBlockedIP(embeddedIPv4(ip))
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if isAlwaysBlockedIP(embeddedIPv4(a)) {
			return true
		}
	}
	return false
}

// Reports whether a URL may be fetched, failing closed on private targets unless the security toggle allows them
func isSafeURL(env *tools.Env, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	if u.User != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return false
	}
	if blockedHostnames[host] {
		return false
	}
	allowPrivate := section(cfgOf(env), "security").Bool("allow_private_urls", false)
	if ip := net.ParseIP(host); ip != nil {
		if isAlwaysBlockedIP(embeddedIPv4(ip)) {
			return false
		}
		return allowPrivate || !isBlockedIP(ip)
	}
	addrs, err := net.LookupIP(host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if isAlwaysBlockedIP(embeddedIPv4(a)) {
			return false
		}
		if !allowPrivate && isBlockedIP(a) {
			return false
		}
	}
	return true
}

// Reports whether a base URL points at loopback, RFC 1918, or LAN internal hosts
func isLocalOrPrivateURL(rawURL string) bool {
	host := urlHostname(rawURL)
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".internal") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback()
}
