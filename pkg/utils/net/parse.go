package netutil

import (
	"math/bits"
	"net/netip"
	"strconv"
	"strings"
)

// Error of text that names no address or network, carrying the text and what is wrong with it
type ParseError struct {
	Input  string
	Reason string
}

// The quoted input followed by the reason
func (e *ParseError) Error() string { return "parse " + strconv.Quote(e.Input) + ": " + e.Reason }

// Network of an address with an optional /length, for IPv4 also a dotted /netmask or /hostmask, a bare address standing for itself alone; leading zeros in the length are accepted, host bits cleared and an IPv6 zone dropped
func ParseNetwork(s string) (netip.Prefix, error) {
	addrText, lengthText, hasLength := strings.Cut(s, "/")
	addr, err := netip.ParseAddr(addrText)
	if err != nil {
		return netip.Prefix{}, &ParseError{Input: s, Reason: "invalid address " + strconv.Quote(addrText)}
	}
	addr = addr.WithZone("")
	length := addr.BitLen()
	if hasLength {
		n, ok := prefixLength(lengthText, addr.BitLen())
		if !ok && addr.Is4() {
			n, ok = maskLength(lengthText)
		}
		if !ok {
			return netip.Prefix{}, &ParseError{Input: s, Reason: "invalid prefix length or mask " + strconv.Quote(lengthText)}
		}
		length = n
	}
	return netip.PrefixFrom(addr, length).Masked(), nil
}

// Prefix length of decimal digits no greater than the family maximum
func prefixLength(s string, maxBits int) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		if n = n*10 + int(s[i]-'0'); n > maxBits {
			return 0, false
		}
	}
	return n, true
}

// Prefix length of a dotted IPv4 netmask, else of a dotted hostmask; 0.0.0.0 and 255.255.255.255 read as netmasks
func maskLength(s string) (int, bool) {
	mask, err := netip.ParseAddr(s)
	if err != nil || !mask.Is4() {
		return 0, false
	}
	a := mask.As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	if n := bits.LeadingZeros32(^v); v == ^uint32(0)<<(32-n) {
		return n, true
	}
	if n := bits.LeadingZeros32(v); ^v == ^uint32(0)<<(32-n) {
		return n, true
	}
	return 0, false
}

// IPv4 address of a numeric host in the lenient spellings name resolvers and browsers accept: one to four dot separated parts, each decimal, octal after a leading 0 or hex after 0x, the last part filling the bytes left; the C library form may be followed by whitespace and anything after it, the browser form may end in one dot and write zero as a bare 0x
func ParseLenientIPv4(s string) (netip.Addr, error) {
	text, browser := s, true
	if i := strings.IndexAny(s, " \t\n\v\f\r"); i >= 0 {
		text, browser = s[:i], false
	} else if len(text) > 1 && text[len(text)-1] == '.' {
		text = text[:len(text)-1]
	}
	var parts [4]uint32
	n := 0
	for part := range strings.SplitSeq(text, ".") {
		if n == len(parts) {
			return netip.Addr{}, &ParseError{Input: s, Reason: "more than four parts"}
		}
		v, ok := ipv4Part(part, browser)
		if !ok {
			return netip.Addr{}, &ParseError{Input: s, Reason: "part " + strconv.Quote(part) + " is not a number"}
		}
		parts[n] = v
		n++
	}
	v := parts[n-1]
	if v > ^uint32(0)>>(8*(n-1)) {
		return netip.Addr{}, &ParseError{Input: s, Reason: "last part out of range"}
	}
	for i, p := range parts[:n-1] {
		if p > 0xff {
			return netip.Addr{}, &ParseError{Input: s, Reason: "part " + strconv.Itoa(i+1) + " out of range"}
		}
		v |= p << (24 - 8*i)
	}
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), nil
}

// Value of one numeric part, an empty hex part counting as zero only in the browser form
func ipv4Part(s string, browser bool) (uint32, bool) {
	base := uint64(10)
	switch {
	case s == "":
		return 0, false
	case len(s) > 1 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X'):
		base, s = 16, s[2:]
		if s == "" {
			return 0, browser
		}
	case len(s) > 1 && s[0] == '0':
		base, s = 8, s[1:]
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		d := digitValue(s[i])
		if d >= base {
			return 0, false
		}
		if v = v*base + d; v > 0xffffffff {
			return 0, false
		}
	}
	return uint32(v), true
}

func digitValue(c byte) uint64 {
	switch {
	case '0' <= c && c <= '9':
		return uint64(c - '0')
	case 'a' <= c && c <= 'f':
		return uint64(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return uint64(c-'A') + 10
	}
	return 16
}
