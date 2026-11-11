package pyipaddress

import (
	"math/bits"
	"net/netip"
	"strconv"
	"strings"
)

// Parses a network like ipaddress.ip_network, netmask and hostmask suffixes included, false on ValueError
func IPNetwork(s string, strict bool) (netip.Prefix, bool) {
	parts := strings.Split(s, "/")
	if len(parts) > 2 {
		return netip.Prefix{}, false
	}
	if prefix, ok := v4Network(parts, strict); ok {
		return prefix, true
	}
	return v6Network(parts, strict)
}

// IPv4Network of the split address and optional mask
func v4Network(parts []string, strict bool) (netip.Prefix, bool) {
	addr, ok := IPAddress(parts[0])
	if !ok || !addr.Is4() {
		return netip.Prefix{}, false
	}
	bitsLen := 32
	if len(parts) == 2 {
		n, ok := prefixFromPrefixString(parts[1], 32)
		if !ok {
			n, ok = v4PrefixFromIPString(parts[1])
		}
		if !ok {
			return netip.Prefix{}, false
		}
		bitsLen = n
	}
	return maskedPrefix(addr, bitsLen, strict)
}

// IPv6Network of the split address and optional prefix length
func v6Network(parts []string, strict bool) (netip.Prefix, bool) {
	addr, ok := IPAddress(parts[0])
	if !ok || !addr.Is6() {
		return netip.Prefix{}, false
	}
	bitsLen := 128
	if len(parts) == 2 {
		n, ok := prefixFromPrefixString(parts[1], 128)
		if !ok {
			return netip.Prefix{}, false
		}
		bitsLen = n
	}
	return maskedPrefix(addr.WithZone(""), bitsLen, strict)
}

// Prefix of the address, refused under strict when host bits are set
func maskedPrefix(addr netip.Addr, bitsLen int, strict bool) (netip.Prefix, bool) {
	prefix := netip.PrefixFrom(addr, bitsLen)
	masked := prefix.Masked()
	if strict && masked.Addr() != addr {
		return netip.Prefix{}, false
	}
	return masked, true
}

// Prefix length of an all ASCII digit string within the family maximum
func prefixFromPrefixString(text string, maxLen int) (int, bool) {
	if text == "" {
		return 0, false
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 || n > maxLen {
		return 0, false
	}
	return n, true
}

// Prefix length of a dotted netmask, else of a dotted hostmask, both ambiguous masks read as netmasks
func v4PrefixFromIPString(text string) (int, bool) {
	addr, ok := IPAddress(text)
	if !ok || !addr.Is4() {
		return 0, false
	}
	raw := addr.As4()
	value := uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3])
	if n, ok := prefixFromIPInt(value); ok {
		return n, true
	}
	return prefixFromIPInt(^value)
}

// Prefix length of a contiguous leading ones mask, false for any other bit pattern
func prefixFromIPInt(value uint32) (int, bool) {
	trailing := bits.TrailingZeros32(value)
	if value == 0 {
		trailing = 32
	}
	prefixLen := 32 - trailing
	if prefixLen == 0 {
		return 0, true
	}
	leading := value >> uint(trailing)
	allOnes := uint32(1)<<uint(prefixLen) - 1
	if prefixLen == 32 {
		allOnes = ^uint32(0)
	}
	return prefixLen, leading == allOnes
}

// Whether the address lies in the network, false across families and blind to an IPv6 scope
func NetworkContains(prefix netip.Prefix, addr netip.Addr) bool {
	return prefix.Contains(addr.WithZone(""))
}
