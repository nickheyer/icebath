package pyurl

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Percent decodes like urllib.parse.unquote, ASCII runs as UTF-8 with replacement and other text kept
func Unquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var out strings.Builder
	var run []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 {
			out.WriteString(pystr.DecodeUTF8Replace(run))
			run = run[:0]
			out.WriteByte(c)
			continue
		}
		if c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			run = append(run, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		run = append(run, c)
	}
	out.WriteString(pystr.DecodeUTF8Replace(run))
	return out.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}
