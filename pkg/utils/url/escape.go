package urlutil

import "strings"

const upperHex = "0123456789ABCDEF"

// Set of ASCII bytes
type byteSet [2]uint64

func (s *byteSet) add(c byte) {
	if c < 0x80 {
		s[c>>6] |= 1 << (c & 63)
	}
}

func (s *byteSet) has(c byte) bool { return c < 0x80 && s[c>>6]&(1<<(c&63)) != 0 }

func unreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

func ishex(c byte) bool { return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F' }

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// s percent-encoded byte by byte with upper case hex, ASCII letters, digits, -._~ and the ASCII bytes of keep left as they are
func Escape(s, keep string) string {
	var kept byteSet
	for i := range len(keep) {
		kept.add(keep[i])
	}
	n := 0
	for i := range len(s) {
		if c := s[i]; !unreserved(c) && !kept.has(c) {
			n++
		}
	}
	if n == 0 {
		return s
	}
	b := make([]byte, 0, len(s)+2*n)
	for i := range len(s) {
		c := s[i]
		if unreserved(c) || kept.has(c) {
			b = append(b, c)
			continue
		}
		b = append(b, '%', upperHex[c>>4], upperHex[c&15])
	}
	return string(b)
}

// s with every %XX escape decoded to its byte, any other percent sign and every other byte kept as it is
func Unescape(s string) string { return unescape(s, false) }

// Lenient percent decoding, a plus sign also becoming a space when plus is set
func unescape(s string, plus bool) string {
	var i int
	if plus {
		i = strings.IndexAny(s, "%+")
	} else {
		i = strings.IndexByte(s, '%')
	}
	if i < 0 {
		return s
	}
	b := make([]byte, i, len(s))
	copy(b, s[:i])
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c == '%' && i+2 < len(s) && ishex(s[i+1]) && ishex(s[i+2]):
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
		case c == '+' && plus:
			b = append(b, ' ')
		default:
			b = append(b, c)
		}
	}
	return string(b)
}
