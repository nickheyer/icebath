package pyipaddress

import (
	"net/netip"
	"strconv"
	"strings"
)

func prefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

var (
	privateV4 = prefixes("0.0.0.0/8", "10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
		"192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"255.255.255.255/32")
	privateV4Exceptions = prefixes("192.0.0.9/32", "192.0.0.10/32")
	privateV6           = prefixes("::1/128", "::/128", "::ffff:0:0/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "2002::/16", "3fff::/20", "fc00::/7", "fe80::/10")
	privateV6Exceptions = prefixes("2001:1::1/128", "2001:1::2/128", "2001:3::/32", "2001:4:112::/48", "2001:20::/28",
		"2001:30::/28")
	linkLocalV4 = netip.MustParsePrefix("169.254.0.0/16")
	linkLocalV6 = netip.MustParsePrefix("fe80::/10")
	loopbackV4  = netip.MustParsePrefix("127.0.0.0/8")
	reservedV4  = netip.MustParsePrefix("240.0.0.0/4")
	reservedV6  = prefixes("::/8", "100::/8", "200::/7", "400::/6", "800::/5", "1000::/4", "4000::/3", "6000::/3", "8000::/3",
		"a000::/3", "c000::/3", "e000::/4", "f000::/5", "f800::/6", "fe00::/9")
)

func within(ip netip.Addr, nets []netip.Prefix) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Address an IPv4 mapped IPv6 address stands for, the bare address otherwise
func mapped(ip netip.Addr) netip.Addr {
	ip = ip.WithZone("")
	if ip.Is4In6() {
		return ip.Unmap()
	}
	return ip
}

// Private classification of CPython 3.13 ipaddress, mapped IPv6 judged by its IPv4 address
func IsPrivate(ip netip.Addr) bool {
	ip = mapped(ip)
	if ip.Is4() {
		return within(ip, privateV4) && !within(ip, privateV4Exceptions)
	}
	return within(ip, privateV6) && !within(ip, privateV6Exceptions)
}

// Loopback classification of CPython 3.13 ipaddress, mapped IPv6 judged by its IPv4 address
func IsLoopback(ip netip.Addr) bool {
	ip = mapped(ip)
	if ip.Is4() {
		return loopbackV4.Contains(ip)
	}
	return ip == netip.IPv6Loopback()
}

// Link local classification of CPython 3.13 ipaddress, mapped IPv6 judged by its IPv4 address
func IsLinkLocal(ip netip.Addr) bool {
	ip = mapped(ip)
	if ip.Is4() {
		return linkLocalV4.Contains(ip)
	}
	return linkLocalV6.Contains(ip)
}

// Reserved classification of CPython 3.13 ipaddress, mapped IPv6 judged by its IPv4 address
func IsReserved(ip netip.Addr) bool {
	ip = mapped(ip)
	if ip.Is4() {
		return reservedV4.Contains(ip)
	}
	return within(ip, reservedV6)
}

// Parses an address like ipaddress.ip_address including an IPv6 scope suffix, false on ValueError
func IPAddress(s string) (netip.Addr, bool) {
	if strings.ContainsAny(s, "[] \t\n") {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	if ip.Is4() && ip.Zone() != "" {
		return netip.Addr{}, false
	}
	return ip, true
}

// Parses dotted IPv4 like glibc inet_aton with decimal, octal and hex parts and short forms
func InetAton(s string) (netip.Addr, bool) {
	var parts []uint64
	rest := s
	for {
		if rest == "" || rest[0] < '0' || rest[0] > '9' {
			return netip.Addr{}, false
		}
		val, n := strtoul(rest)
		if n < 0 {
			return netip.Addr{}, false
		}
		rest = rest[n:]
		if rest != "" && rest[0] == '.' {
			if len(parts) > 2 || val > 0xff {
				return netip.Addr{}, false
			}
			parts = append(parts, val)
			rest = rest[1:]
			continue
		}
		if rest != "" && !strings.ContainsRune(" \t\n\v\f\r", rune(rest[0])) {
			return netip.Addr{}, false
		}
		limits := []uint64{0xffffffff, 0xffffff, 0xffff, 0xff}
		if val > limits[len(parts)] {
			return netip.Addr{}, false
		}
		var out uint64
		for i, p := range parts {
			out |= p << (24 - 8*uint(i))
		}
		out |= val
		return netip.AddrFrom4([4]byte{byte(out >> 24), byte(out >> 16), byte(out >> 8), byte(out)}), true
	}
}

// Reads one strtoul base 0 number, returning its value and length or a negative length on overflow
func strtoul(s string) (uint64, int) {
	base, i := 10, 0
	if s[0] == '0' {
		base, i = 8, 1
		if len(s) > 2 && (s[1] == 'x' || s[1] == 'X') && isDigit(s[2], 16) {
			base, i = 16, 2
		}
	}
	j := i
	for j < len(s) && isDigit(s[j], base) {
		j++
	}
	if j == i {
		return 0, i
	}
	val, err := strconv.ParseUint(s[i:j], base, 64)
	if err != nil || val > 0xffffffff {
		return 0, -1
	}
	return val, j
}

func isDigit(c byte, base int) bool {
	switch {
	case c >= '0' && c <= '7':
		return true
	case c == '8' || c == '9':
		return base >= 10
	case base == 16 && ((c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')):
		return true
	}
	return false
}
