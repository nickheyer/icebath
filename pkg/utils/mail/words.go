package mailutil

import "strings"

// Header text with RFC 2047 encoded words decoded to UTF-8: adjacent words in one character set joined before decoding so characters split across them survive, white space between encoded words dropped, base64 without padding accepted, malformed words and other text kept as they are with raw 8 bit text read as UTF-8 or else Windows-1252
func DecodeHeader(value string) string {
	if !strings.Contains(value, "=?") {
		return plainText(value)
	}
	var b strings.Builder
	b.Grow(len(value))
	var pending []byte
	pendingCharset := ""
	flush := func() {
		if len(pending) > 0 {
			b.WriteString(decodeText(pending, pendingCharset))
		}
		pending = pending[:0]
	}
	afterWord := false
	rest := value
	for {
		start, end, charset, data, ok := nextEncodedWord(rest)
		if start < 0 {
			break
		}
		gap := rest[:start]
		if !ok {
			flush()
			b.WriteString(plainText(rest[:end]))
			rest, afterWord = rest[end:], false
			continue
		}
		if !afterWord || strings.Trim(gap, " \t") != "" {
			flush()
			b.WriteString(plainText(gap))
		}
		if len(pending) > 0 && !strings.EqualFold(charset, pendingCharset) {
			flush()
		}
		pending = append(pending, data...)
		pendingCharset = charset
		rest, afterWord = rest[end:], true
	}
	flush()
	b.WriteString(plainText(rest))
	return b.String()
}

// Next encoded word in s with its character set and decoded bytes: start is -1 when s holds no complete one, and ok is false for an =? that opens no well formed word, end then marking how far the text is kept as it is
func nextEncodedWord(s string) (start, end int, charset string, data []byte, ok bool) {
	start = strings.Index(s, "=?")
	if start < 0 {
		return -1, 0, "", nil, false
	}
	rest := s[start+2:]
	q := strings.IndexByte(rest, '?')
	if q < 0 {
		return -1, 0, "", nil, false
	}
	if q+2 >= len(rest) || rest[q+2] != '?' || strings.ContainsAny(rest[:q], " \t") {
		return start, start + 2, "", nil, false
	}
	encoded := rest[q+3:]
	closing := strings.Index(encoded, "?=")
	if closing < 0 {
		// no ?= anywhere further on, so no later word can be complete either, and scanning on would be quadratic
		return -1, 0, "", nil, false
	}
	end = start + 2 + q + 3 + closing + 2
	text := encoded[:closing]
	switch rest[q+1] {
	case 'B', 'b':
		data = decodeBase64([]byte(text))
	case 'Q', 'q':
		data = decodeQWord(text)
	default:
		return start, end, "", nil, false
	}
	charset, _, _ = strings.Cut(rest[:q], "*")
	return start, end, charset, data, true
}

// Bytes of the text of a Q encoded word: underscores as spaces, =XX escapes decoded and a = that starts none kept
func decodeQWord(text string) []byte {
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '_':
			out = append(out, ' ')
		case c == '=' && i+2 < len(text) && isHex(text[i+1]) && isHex(text[i+2]):
			out = append(out, unhex(text[i+1])<<4|unhex(text[i+2]))
			i += 2
		default:
			out = append(out, c)
		}
	}
	return out
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
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
