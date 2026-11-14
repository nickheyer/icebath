package vault

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Reports whether r is whitespace, the Unicode White_Space set plus the ASCII separators 0x1c through 0x1f
func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, ' ', 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Text without leading and trailing whitespace
func trimSpace(s string) string { return strings.TrimFunc(s, isSpace) }

// Lowercase by the full Unicode mapping, final sigma and the two rune form of dotted capital I included
func lowerFull(s string) string { return cases.Lower(language.Und).String(s) }

// Uppercase by the full Unicode mapping, sharp s and ligatures expanding to several letters
func upperFull(s string) string { return cases.Upper(language.Und).String(s) }

// First n runes of s
func head(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// Reports whether s is non-empty and every rune is a decimal digit
func allDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsDigit(r) }) < 0
}

// Reports whether every byte of s is ASCII
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// Quoted literal of s the way the reference prints values in messages, single quotes unless s holds only that kind
func quote(s string) string {
	q := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\udc%02x`, s[i])
		case r == '\\' || r == rune(q):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
		i += size
	}
	b.WriteByte(q)
	return b.String()
}

// Text of command output with each maximal invalid UTF-8 subpart replaced by U+FFFD and CRLF or CR read as LF
func outputText(b []byte) string {
	text := lossyUTF8(b)
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// UTF-8 text of b with one U+FFFD per maximal invalid subpart
func lossyUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			sb.Write(b[i : i+size])
			i += size
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += invalidSubpart(b[i:])
	}
	return sb.String()
}

// Bytes of the maximal subpart of an ill formed sequence at the head of b, at least one
func invalidSubpart(b []byte) int {
	need, lo, hi := 0, byte(0x80), byte(0xbf)
	switch c := b[0]; {
	case c >= 0xc2 && c <= 0xdf:
		need = 1
	case c == 0xe0:
		need, lo = 2, 0xa0
	case c == 0xed:
		need, hi = 2, 0x9f
	case c >= 0xe1 && c <= 0xef:
		need = 2
	case c == 0xf0:
		need, lo = 3, 0x90
	case c >= 0xf1 && c <= 0xf3:
		need = 3
	case c == 0xf4:
		need, hi = 3, 0x8f
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(b); k++ {
		low, high := byte(0x80), byte(0xbf)
		if k == 0 {
			low, high = lo, hi
		}
		if b[n] < low || b[n] > high {
			break
		}
		n++
	}
	return n
}
