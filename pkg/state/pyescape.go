package state

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
)

// Python str.lower with the full Unicode mapping and final sigma rule
func pyLower(s string) string { return cases.Lower(language.Und).String(s) }

// Python str.upper with the full Unicode mapping that expands characters such as the sharp s
func pyUpper(s string) string { return cases.Upper(language.Und).String(s) }

// Code point count of a string whose lone surrogates are written in generalized UTF-8
func pyLenWTF8(s string) int {
	n := 0
	for i := 0; i < len(s); n++ {
		if _, ok := wtf8Surrogate(s[i:]); ok {
			i += 3
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return n
}

// Appends a code point, a lone surrogate written in its generalized UTF-8 form
func writeCodePoint(b *strings.Builder, r rune) {
	if r >= 0xD800 && r <= 0xDFFF {
		b.WriteByte(byte(0xE0 | r>>12))
		b.WriteByte(byte(0x80 | (r>>6)&0x3F))
		b.WriteByte(byte(0x80 | r&0x3F))
		return
	}
	b.WriteRune(r)
}

// Bytes of str.encode latin-1 with errors ignore, code points past 0xFF dropped
func latin1Ignore(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if _, ok := wtf8Surrogate(s[i:]); ok {
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r <= 0xFF && !(r == utf8.RuneError && size == 1) {
			out = append(out, byte(r))
		}
		i += size
	}
	return out
}

// The UnicodeDecodeError text Python gives for a failed unicode_escape decode
func unicodeEscapeError(data []byte, start, end int, reason string) error {
	if end == start+1 {
		return &pyConvError{"UnicodeDecodeError", fmt.Sprintf("'unicodeescape' codec can't decode byte 0x%02x in position %d: %s", data[start], start, reason)}
	}
	return &pyConvError{"UnicodeDecodeError", fmt.Sprintf("'unicodeescape' codec can't decode bytes in position %d-%d: %s", start, end-1, reason)}
}

// Value of count hex digits starting at offset i, the offending offset when one is missing or not hex
func hexEscapeValue(data []byte, i, count int) (rune, int, bool) {
	var ch rune
	for ; count > 0; i, count = i+1, count-1 {
		if i >= len(data) {
			return 0, i, false
		}
		c := data[i]
		ch <<= 4
		switch {
		case c >= '0' && c <= '9':
			ch += rune(c - '0')
		case c >= 'a' && c <= 'f':
			ch += rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			ch += rune(c-'A') + 10
		default:
			return 0, i, false
		}
	}
	return ch, i, true
}

var simpleEscapes = map[byte]rune{'\\': '\\', '\'': '\'', '"': '"', 'b': '\b', 'f': '\f', 't': '\t', 'n': '\n', 'r': '\r', 'v': '\v', 'a': '\a'}

// String bytes.decode unicode_escape yields, the strict UnicodeDecodeError on a malformed escape
func pyUnicodeEscapeDecode(data []byte) (string, error) {
	var b strings.Builder
	for s := 0; s < len(data); {
		if data[s] != '\\' {
			b.WriteRune(rune(data[s]))
			s++
			continue
		}
		start := s
		s++
		if s >= len(data) {
			return "", unicodeEscapeError(data, start, s, `\ at end of string`)
		}
		c := data[s]
		s++
		if r, ok := simpleEscapes[c]; ok {
			b.WriteRune(r)
			continue
		}
		switch {
		case c == '\n':
		case c >= '0' && c <= '7':
			ch := rune(c - '0')
			for n := 0; n < 2 && s < len(data) && data[s] >= '0' && data[s] <= '7'; n++ {
				ch = ch<<3 + rune(data[s]-'0')
				s++
			}
			writeCodePoint(&b, ch)
		case c == 'x' || c == 'u' || c == 'U':
			count, reason := 2, `truncated \xXX escape`
			if c == 'u' {
				count, reason = 4, `truncated \uXXXX escape`
			} else if c == 'U' {
				count, reason = 8, `truncated \UXXXXXXXX escape`
			}
			ch, next, ok := hexEscapeValue(data, s, count)
			if !ok {
				return "", unicodeEscapeError(data, start, next, reason)
			}
			s = next
			if ch > utf8.MaxRune || ch < 0 {
				return "", unicodeEscapeError(data, start, s, "illegal Unicode character")
			}
			writeCodePoint(&b, ch)
		case c == 'N':
			reason := `malformed \N character escape`
			if s >= len(data) {
				return "", unicodeEscapeError(data, start, s, reason)
			}
			if data[s] == '{' {
				s++
				nameStart := s
				for s < len(data) && data[s] != '}' {
					s++
				}
				if s >= len(data) {
					return "", unicodeEscapeError(data, start, s, reason)
				}
				if s > nameStart {
					s++
					if r, ok := pyunicode.Lookup(string(data[nameStart : s-1])); ok {
						writeCodePoint(&b, r)
						continue
					}
					reason = "unknown Unicode character name"
				}
			}
			return "", unicodeEscapeError(data, start, s, reason)
		default:
			b.WriteByte('\\')
			b.WriteRune(rune(c))
		}
	}
	return b.String(), nil
}
