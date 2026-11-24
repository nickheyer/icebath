package urlutil

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Error for an authority whose host cannot be read: unbalanced brackets, brackets around anything but an IPv6 or IPvFuture literal, or text NFKC turns into delimiters
var ErrInvalidHost = errors.New("invalid host")

// Host and port text of an authority, user info dropped and the brackets of an IP literal removed
func hostPort(authority string) (host, port string, err error) {
	hostinfo := authority[strings.LastIndexByte(authority, '@')+1:]
	if !strings.HasPrefix(hostinfo, "[") {
		if strings.ContainsAny(hostinfo, "[]") {
			return "", "", fmt.Errorf("%w: unbalanced or misplaced brackets in %q", ErrInvalidHost, hostinfo)
		}
		host, port, _ = strings.Cut(hostinfo, ":")
		return host, port, nil
	}
	end := strings.IndexByte(hostinfo, ']')
	if end < 0 {
		return "", "", fmt.Errorf("%w: unbalanced or misplaced brackets in %q", ErrInvalidHost, hostinfo)
	}
	host, rest := hostinfo[1:end], hostinfo[end+1:]
	if rest != "" {
		if rest[0] != ':' {
			return "", "", fmt.Errorf("%w: text after the closing bracket in %q", ErrInvalidHost, hostinfo)
		}
		port = rest[1:]
	}
	if err := checkLiteral(host); err != nil {
		return "", "", err
	}
	return host, port, nil
}

// Refuses a bracketed host that is not an IPv6 address, zone allowed, or an IPvFuture literal v<hex>.<text>
func checkLiteral(s string) error {
	if s != "" && (s[0] == 'v' || s[0] == 'V') {
		dot := strings.IndexByte(s, '.')
		valid := dot >= 2 && dot < len(s)-1
		for i := 1; valid && i < dot; i++ {
			valid = ishex(s[i])
		}
		if !valid {
			return fmt.Errorf("%w: %q is not a valid IPvFuture literal", ErrInvalidHost, s)
		}
		return nil
	}
	addr, err := netip.ParseAddr(s)
	_, zone, _ := strings.Cut(s, "%")
	switch {
	case err != nil || strings.Contains(zone, "%"):
		return fmt.Errorf("%w: %q is not an IPv6 address", ErrInvalidHost, s)
	case addr.Is4():
		return fmt.Errorf("%w: IPv4 address %q in brackets", ErrInvalidHost, s)
	}
	return nil
}

// Host lowercased with an IPv6 zone after % kept as written
func lowerHost(host string) string {
	if h, zone, ok := strings.Cut(host, "%"); ok {
		return strings.ToLower(h) + "%" + zone
	}
	return strings.ToLower(host)
}

// Host as it goes in a URL authority: surrounding space trimmed, an IPv6 literal bracketed with its zone percent escaped as %25
func FormatHost(host string) string {
	host = strings.TrimSpace(host)
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		if inner := host[1 : len(host)-1]; strings.Contains(inner, ":") {
			return "[" + strings.ReplaceAll(inner, "%", "%25") + "]"
		}
		return host
	}
	if strings.Contains(host, ":") {
		return "[" + strings.ReplaceAll(host, "%", "%25") + "]"
	}
	return host
}
