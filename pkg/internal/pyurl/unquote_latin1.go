package pyurl

import "strings"

// Percent decodes like urllib.parse.unquote with encoding latin-1, each decoded byte becoming the same code point
func UnquoteLatin1(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			out.WriteByte(c)
			continue
		}
		if c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			out.WriteRune(rune(unhex(s[i+1])<<4 | unhex(s[i+2])))
			i += 2
			continue
		}
		out.WriteByte(c)
	}
	return out.String()
}
