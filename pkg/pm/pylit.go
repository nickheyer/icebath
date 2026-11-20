package pm

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
)

// Appends a code point, a lone surrogate written in its generalized UTF-8 form
func appendCodePoint(b *strings.Builder, r rune) {
	if r >= 0xD800 && r <= 0xDFFF {
		b.WriteByte(byte(0xE0 | r>>12))
		b.WriteByte(byte(0x80 | (r>>6)&0x3F))
		b.WriteByte(byte(0x80 | r&0x3F))
		return
	}
	b.WriteRune(r)
}

// Value of a quoted string evaluated as a Python string literal, ok false where Python raises
func pyStringLiteral(text string) (string, bool) {
	src := []rune(text)
	if len(src) < 2 || strings.ContainsRune(text, 0) {
		return "", false
	}
	body := src[1 : len(src)-1]
	var b strings.Builder
	hexRun := func(at, n int) (rune, bool) {
		if at+n > len(body) {
			return 0, false
		}
		v, err := strconv.ParseUint(string(body[at:at+n]), 16, 32)
		if err != nil {
			return 0, false
		}
		return rune(v), true
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '\n' || c == '\r' {
			return "", false
		}
		if c != '\\' {
			b.WriteRune(c)
			continue
		}
		if i+1 == len(body) {
			return "", false
		}
		i++
		switch e := body[i]; e {
		case '\n':
		case '\r':
			if i+1 < len(body) && body[i+1] == '\n' {
				i++
			}
		case '\\', '\'', '"':
			b.WriteRune(e)
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v := e - '0'
			for k := 0; k < 2 && i+1 < len(body) && body[i+1] >= '0' && body[i+1] <= '7'; k++ {
				i++
				v = v*8 + body[i] - '0'
			}
			appendCodePoint(&b, v)
		case 'x', 'u', 'U':
			n := map[rune]int{'x': 2, 'u': 4, 'U': 8}[e]
			v, ok := hexRun(i+1, n)
			if !ok || v > utf8.MaxRune {
				return "", false
			}
			i += n
			appendCodePoint(&b, v)
		case 'N':
			if i+1 >= len(body) || body[i+1] != '{' {
				return "", false
			}
			end := i + 2
			for end < len(body) && body[end] != '}' {
				end++
			}
			if end >= len(body) || end == i+2 {
				return "", false
			}
			r, ok := pyunicode.Lookup(string(body[i+2 : end]))
			if !ok {
				return "", false
			}
			appendCodePoint(&b, r)
			i = end
		default:
			b.WriteByte('\\')
			b.WriteRune(e)
		}
	}
	return b.String(), true
}
