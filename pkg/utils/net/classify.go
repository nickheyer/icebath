package netutil

import (
	"net/netip"
	"sync"
)

type ipv6Blocks struct {
	private, exceptions []netip.Prefix
}

// IPv6 blocks the IANA special-purpose registry marks not globally reachable, and the globally reachable blocks inside them
var ipv6Special = sync.OnceValue(func() *ipv6Blocks {
	return &ipv6Blocks{
		private: prefixes("::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "100:0:0:1::/64", "2001::/23", "2001:db8::/32",
			"2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10"),
		exceptions: prefixes("2001:1::1/128", "2001:1::2/128", "2001:3::/32", "2001:4:112::/48", "2001:20::/28", "2001:30::/28"),
	}
})

func prefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

func within(ip netip.Addr, blocks []netip.Prefix) bool {
	for _, b := range blocks {
		if b.Contains(ip) {
			return true
		}
	}
	return false
}

// Whether the address lies in a block the IANA special-purpose registries mark not globally reachable (RFC 1918, unique local, loopback, link local, this network, protocol assignments, documentation, benchmarking, 6a44 relay, 6to4, Teredo, local NAT64, discard, dummy prefix, SRv6, reserved and broadcast space), the IPv6 protocol assignments block 2001::/23 counted whole apart from its PCP, TURN, AMT, AS112, ORCHIDv2 and drone ID entries; an IPv4-mapped address, and one in the NAT64 well-known prefix 64:ff9b::/96 or the IPv4-translated prefix ::ffff:0:0:0/96, is judged by the IPv4 address it reaches, a zone is ignored, shared space 100.64.0.0/10 is left to IsShared
func IsPrivate(ip netip.Addr) bool {
	ip = translatedIPv4(ip)
	if ip.Is4() {
		return private4(ip.As4())
	}
	blocks := ipv6Special()
	return within(ip, blocks.private) && !within(ip, blocks.exceptions)
}

// IPv4 address an IPv6 address carries to a translator: an IPv4-mapped address, or one in the NAT64 well-known prefix 64:ff9b::/96 (RFC 6052) or the IPv4-translated prefix ::ffff:0:0:0/96 (RFC 2765), which stateless and stateful translators turn into the IPv4 address in the low 32 bits; any other address as it is, zone dropped
func translatedIPv4(ip netip.Addr) netip.Addr {
	ip = ip.Unmap().WithZone("")
	if !ip.Is6() {
		return ip
	}
	a := ip.As16()
	nat64 := a[0] == 0x00 && a[1] == 0x64 && a[2] == 0xff && a[3] == 0x9b && a[4]|a[5]|a[6]|a[7]|a[8]|a[9]|a[10]|a[11] == 0
	translated := a[0]|a[1]|a[2]|a[3]|a[4]|a[5]|a[6]|a[7] == 0 && a[8] == 0xff && a[9] == 0xff && a[10]|a[11] == 0
	if nat64 || translated {
		return netip.AddrFrom4([4]byte(a[12:16]))
	}
	return ip
}

// IPv4 blocks of the special-purpose registry that are not globally reachable
func private4(a [4]byte) bool {
	switch a[0] {
	case 0, 10, 127:
		return true
	case 169:
		return a[1] == 254
	case 172:
		return a[1]&0xf0 == 16
	case 192:
		return a[1] == 168 || (a[1] == 0 && (a[2] == 2 || (a[2] == 0 && a[3] != 9 && a[3] != 10))) || (a[1] == 88 && a[2] == 99 && a[3] == 2)
	case 198:
		return a[1]&0xfe == 18 || (a[1] == 51 && a[2] == 100)
	case 203:
		return a[1] == 0 && a[2] == 113
	}
	return a[0] >= 240
}

// Whether the address lies in space the IETF holds in reserve: IPv4 240.0.0.0/4 with the broadcast address, IPv6 outside global unicast 2000::/3, unique local, link local, site local and multicast space; an IPv4-mapped address is judged by its IPv4 address
func IsReserved(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is4() {
		return ip.As4()[0] >= 240
	}
	if !ip.Is6() {
		return false
	}
	a := ip.As16()
	switch {
	case a[0]&0xe0 == 0x20, a[0]&0xfe == 0xfc, a[0] == 0xfe && a[1]&0x80 != 0, a[0] == 0xff:
		return false
	}
	return true
}

// Whether the address lies in the shared address space 100.64.0.0/10 of carrier-grade NAT, an IPv4-mapped, NAT64 well-known prefix or IPv4-translated address judged by the IPv4 address it reaches
func IsShared(ip netip.Addr) bool {
	ip = translatedIPv4(ip)
	if !ip.Is4() {
		return false
	}
	a := ip.As4()
	return a[0] == 100 && a[1]&0xc0 == 64
}
