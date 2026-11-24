package textutil

import "strings"

// s with every CRLF pair and every lone CR turned into LF
func NormalizeNewlines(s string) string {
	i := strings.IndexByte(s, '\r')
	if i < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i >= 0 {
		b.WriteString(s[:i])
		b.WriteByte('\n')
		s = s[i+1:]
		if s != "" && s[0] == '\n' {
			s = s[1:]
		}
		i = strings.IndexByte(s, '\r')
	}
	b.WriteString(s)
	return b.String()
}
