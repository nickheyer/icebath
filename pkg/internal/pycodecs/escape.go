package pycodecs

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
)

// Value of count hex digits from offset i, the offset of the first missing or non hex digit on failure
func hexEscapeValue(data []byte, i, count int) (int64, int, bool) {
	var ch int64
	for ; count > 0; i, count = i+1, count-1 {
		if i >= len(data) {
			return 0, i, false
		}
		c := data[i]
		ch <<= 4
		switch {
		case c >= '0' && c <= '9':
			ch += int64(c - '0')
		case c >= 'a' && c <= 'f':
			ch += int64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			ch += int64(c-'A') + 10
		default:
			return 0, i, false
		}
	}
	return ch, i, true
}

var simpleEscapes = map[byte]byte{'\\': '\\', '\'': '\'', '"': '"', 'b': '\b', 'f': '\f', 't': '\t', 'n': '\n', 'r': '\r', 'v': '\v', 'a': '\a'}

// unicode_escape decode of CPython's _PyUnicode_DecodeUnicodeEscapeInternal2, replace writing U+FFFD and resuming after the error
func decodeUnicodeEscape(data []byte, replace bool) (string, error) {
	var b strings.Builder
	s := 0
	// Raises the error over start to end, or writes U+FFFD and resumes at end
	fail := func(start, end int, reason string) error {
		if !replace {
			return decodeErr("unicodeescape", data, start, end, reason)
		}
		b.WriteRune(0xfffd)
		s = end
		return nil
	}
	for s < len(data) {
		if data[s] != '\\' {
			b.WriteRune(rune(data[s]))
			s++
			continue
		}
		start := s
		s++
		if s >= len(data) {
			if err := fail(start, s, `\ at end of string`); err != nil {
				return "", err
			}
			continue
		}
		c := data[s]
		s++
		if r, ok := simpleEscapes[c]; ok {
			b.WriteByte(r)
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
				if err := fail(start, next, reason); err != nil {
					return "", err
				}
				continue
			}
			s = next
			if ch > 0x10ffff {
				if err := fail(start, s, "illegal Unicode character"); err != nil {
					return "", err
				}
				continue
			}
			writeCodePoint(&b, rune(ch))
		case c == 'N':
			reason := `malformed \N character escape`
			if s < len(data) && data[s] == '{' {
				s++
				nameStart := s
				for s < len(data) && data[s] != '}' {
					s++
				}
				if s < len(data) && s > nameStart {
					s++
					if r, ok := pyunicode.Lookup(string(data[nameStart : s-1])); ok {
						writeCodePoint(&b, r)
						continue
					}
					reason = "unknown Unicode character name"
				}
			}
			if err := fail(start, s, reason); err != nil {
				return "", err
			}
		default:
			b.WriteByte('\\')
			b.WriteRune(rune(c))
		}
	}
	return b.String(), nil
}

// raw_unicode_escape decode of CPython, only backslash u and U escapes read and a trailing backslash kept
func decodeRawUnicodeEscape(data []byte, replace bool) (string, error) {
	var b strings.Builder
	s := 0
	for s < len(data) {
		c := data[s]
		s++
		if c != '\\' || s >= len(data) {
			b.WriteRune(rune(c))
			continue
		}
		start := s - 1
		c = data[s]
		s++
		count, reason := 0, ""
		switch c {
		case 'u':
			count, reason = 4, `truncated \uXXXX escape`
		case 'U':
			count, reason = 8, `truncated \UXXXXXXXX escape`
		default:
			b.WriteByte('\\')
			b.WriteRune(rune(c))
			continue
		}
		ch, next, ok := hexEscapeValue(data, s, count)
		s = next
		if ok && ch > 0x10ffff {
			ok, reason = false, `\Uxxxxxxxx out of range`
		}
		if !ok {
			if !replace {
				return "", decodeErr("rawunicodeescape", data, start, s, reason)
			}
			b.WriteRune(0xfffd)
			continue
		}
		writeCodePoint(&b, rune(ch))
	}
	return b.String(), nil
}

// unicode_escape encode of PyUnicode_AsUnicodeEscapeString over code points
func encodeUnicodeEscape(text string) string {
	var b strings.Builder
	for _, r := range textRunes(text) {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || (r >= 0x7f && r <= 0xff):
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f:
			b.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	return b.String()
}

// raw_unicode_escape encode of PyUnicode_AsRawUnicodeEscapeString, latin-1 bytes kept and the rest escaped
func encodeRawUnicodeEscape(text string) string {
	var b strings.Builder
	for _, r := range textRunes(text) {
		switch {
		case r < 0x100:
			b.WriteByte(byte(r))
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	return b.String()
}
