package pyurl

import "strings"

// Percent encodes UTF-8 bytes like urllib.parse.quote, keeping letters, digits, _.-~ and the safe set
func Quote(s, safe string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("_.-~", c) >= 0 ||
			(c < 0x80 && strings.IndexByte(safe, c) >= 0) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}
