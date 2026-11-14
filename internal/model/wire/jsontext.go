package wire

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// Separator and escaping conventions of one piece of JSON text
type layout uint8

const (
	laySpaced layout = 1 << iota
	layASCII
	layHTML
)

const hexDigits = "0123456789abcdef"

// Appends the separator between members or elements
func (l layout) comma(dst []byte) []byte {
	if l&laySpaced != 0 {
		return append(dst, ',', ' ')
	}
	return append(dst, ',')
}

// Appends the separator between a key and its value
func (l layout) colon(dst []byte) []byte {
	if l&laySpaced != 0 {
		return append(dst, ':', ' ')
	}
	return append(dst, ':')
}

// Guesses the conventions a JSON text was written with from its separators, raw bytes and escapes
func detectLayout(data []byte, spaced bool) layout {
	var l layout
	if spaced {
		l |= laySpaced
	}
	high, markup, wide := false, false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c >= utf8.RuneSelf {
			high = true
			continue
		}
		if c != '\\' {
			continue
		}
		i++
		if i+4 < len(data) && data[i] == 'u' {
			switch v, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16); {
			case err != nil:
			case v == '<' || v == '>' || v == '&':
				markup = true
			case v >= utf8.RuneSelf && v != 0x2028 && v != 0x2029:
				wide = true
			}
		}
	}
	switch {
	case markup:
		l |= layHTML
	case !high && (spaced || wide):
		l |= layASCII
	}
	return l
}

// Appends s as a JSON string, invalid UTF-8 becoming U+FFFD
func appendString(dst []byte, s string, l layout) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			plain := c >= 0x20 && c != '"' && c != '\\'
			if c == 0x7f && l&layASCII != 0 {
				plain = false
			}
			if l&layHTML != 0 && (c == '<' || c == '>' || c == '&') {
				plain = false
			}
			if plain {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			dst = append(dst, s[start:i]...)
			dst = appendHex4(dst, utf8.RuneError)
		case l&layASCII != 0:
			dst = append(dst, s[start:i]...)
			dst = appendEscapedRune(dst, r)
		case r == 0x2028 || r == 0x2029:
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0xf])
		default:
			i += size
			continue
		}
		i += size
		start = i
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// Appends a rune as one \u escape or a surrogate pair of them
func appendEscapedRune(dst []byte, r rune) []byte {
	if r > 0xffff {
		r -= 0x10000
		dst = appendHex4(dst, 0xd800+(r>>10))
		return appendHex4(dst, 0xdc00+(r&0x3ff))
	}
	return appendHex4(dst, r)
}

// Appends \u followed by four lowercase hex digits
func appendHex4(dst []byte, v rune) []byte {
	return append(dst, '\\', 'u', hexDigits[(v>>12)&0xf], hexDigits[(v>>8)&0xf], hexDigits[(v>>4)&0xf], hexDigits[v&0xf])
}

// Trims the four JSON whitespace bytes from both ends
func trimJSON(data []byte) []byte {
	i, j := 0, len(data)
	for i < j && isJSONSpace(data[i]) {
		i++
	}
	for j > i && isJSONSpace(data[j-1]) {
		j--
	}
	return data[i:j]
}

// Reports whether c is whitespace the JSON grammar allows between tokens
func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// Index of the first byte at or after i that is not JSON whitespace
func skipSpace(data []byte, i int) int {
	for i < len(data) && isJSONSpace(data[i]) {
		i++
	}
	return i
}

// Index just past the string token opening at i in valid JSON
func skipString(data []byte, i int) int {
	for j := i + 1; j < len(data); j++ {
		switch data[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return len(data)
}

// Index just past the value starting at i in valid JSON
func skipValue(data []byte, i int) int {
	if i >= len(data) {
		return len(data)
	}
	switch data[i] {
	case '"':
		return skipString(data, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(data); j++ {
			switch data[j] {
			case '"':
				j = skipString(data, j) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return len(data)
	}
	for j := i; j < len(data); j++ {
		switch data[j] {
		case ',', ']', '}', ' ', '\t', '\n', '\r':
			return j
		}
	}
	return len(data)
}

// Text of a JSON string token
func decodeString(tok []byte) string {
	if len(tok) >= 2 && bytes.IndexByte(tok, '\\') < 0 && utf8.Valid(tok) {
		return string(tok[1 : len(tok)-1])
	}
	var s string
	if json.Unmarshal(tok, &s) != nil {
		return ""
	}
	return s
}

// Text of raw when it is a JSON string
func stringValue(raw []byte) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	return decodeString(raw), true
}

// Byte range of one member value or element inside its container text
type span struct {
	key        string
	start, end int
}

// Members of a valid JSON object in order, with its separator style and whether that style is uniform
func splitObject(data []byte) (spans []span, l layout, uniform bool) {
	uniform = true
	spaced, decided := false, false
	type keyTok struct{ start, end int }
	var toks []keyTok
	i := 1
	for first := true; ; first = false {
		gapStart := i
		i = skipSpace(data, i)
		if i >= len(data) || data[i] == '}' {
			if i != gapStart {
				uniform = false
			}
			break
		}
		if first {
			uniform = uniform && i == gapStart
		} else {
			uniform = uniform && gapIs(data[gapStart:i], spaced)
		}
		ks := i
		i = skipString(data, i)
		toks = append(toks, keyTok{ks, i})
		key := decodeString(data[ks:i])
		j := skipSpace(data, i)
		uniform = uniform && j == i
		i = min(j+1, len(data))
		gapStart = i
		i = skipSpace(data, i)
		gap := data[gapStart:i]
		if !decided {
			spaced, decided = len(gap) == 1 && gap[0] == ' ', true
		}
		uniform = uniform && gapIs(gap, spaced)
		vs := i
		i = skipValue(data, i)
		spans = append(spans, span{key, vs, i})
		j = skipSpace(data, i)
		uniform = uniform && j == i
		i = j
		if i >= len(data) || data[i] != ',' {
			break
		}
		i++
	}
	l = detectLayout(data, spaced)
	var scratch []byte
	var seen map[string]struct{}
	if len(spans) > 16 {
		seen = make(map[string]struct{}, len(spans))
	}
	for n, sp := range spans {
		scratch = appendString(scratch[:0], sp.key, l)
		if !bytes.Equal(scratch, data[toks[n].start:toks[n].end]) {
			uniform = false
		}
		if seen != nil {
			if _, dup := seen[sp.key]; dup {
				uniform = false
			}
			seen[sp.key] = struct{}{}
			continue
		}
		for _, earlier := range spans[:n] {
			uniform = uniform && earlier.key != sp.key
		}
	}
	return spans, l, uniform
}

// Reports whether a gap between tokens is exactly what the separator style writes
func gapIs(gap []byte, spaced bool) bool {
	if spaced {
		return len(gap) == 1 && gap[0] == ' '
	}
	return len(gap) == 0
}

// Elements of a valid JSON array in order
func splitArray(data []byte) []span {
	var spans []span
	i := 1
	for {
		i = skipSpace(data, i)
		if i >= len(data) || data[i] == ']' {
			return spans
		}
		vs := i
		i = skipValue(data, i)
		spans = append(spans, span{start: vs, end: i})
		i = skipSpace(data, i)
		if i >= len(data) || data[i] != ',' {
			return spans
		}
		i++
	}
}

// Last entry under a key in a valid JSON object, false when the text is no object or lacks it
func objectMember(data []byte, key string) (json.RawMessage, bool) {
	data = trimJSON(data)
	if len(data) == 0 || data[0] != '{' {
		return nil, false
	}
	var found json.RawMessage
	for i := 1; ; i++ {
		i = skipSpace(data, i)
		if i >= len(data) || data[i] != '"' {
			break
		}
		tok := data[i:skipString(data, i)]
		i = min(skipSpace(data, i+len(tok))+1, len(data))
		start := skipSpace(data, i)
		i = skipValue(data, start)
		if tokenIs(tok, key) {
			found = data[start:i]
		}
		i = skipSpace(data, i)
		if i >= len(data) || data[i] != ',' {
			break
		}
	}
	return found, len(found) > 0
}

// Reports whether a JSON string token spells the given text
func tokenIs(tok []byte, text string) bool {
	if len(tok) >= 2 && bytes.IndexByte(tok, '\\') < 0 {
		return string(tok[1:len(tok)-1]) == text
	}
	return decodeString(tok) == text
}

// Appends the elements a function writes as a JSON array
func appendArray(dst []byte, n int, l layout, elem func(dst []byte, i int) []byte) []byte {
	dst = append(dst, '[')
	for i := 0; i < n; i++ {
		if i > 0 {
			dst = l.comma(dst)
		}
		dst = elem(dst, i)
	}
	return append(dst, ']')
}

// Appends raw JSON values as an array
func appendRawArray(dst []byte, items []json.RawMessage, l layout) []byte {
	return appendArray(dst, len(items), l, func(dst []byte, i int) []byte {
		return append(dst, items[i]...)
	})
}

// Elements of raw when it is a JSON array, each sharing one private copy
func rawElements(raw []byte) ([]json.RawMessage, bool) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	own := bytes.Clone(raw)
	spans := splitArray(own)
	out := make([]json.RawMessage, len(spans))
	for i, sp := range spans {
		out[i] = own[sp.start:sp.end:sp.end]
	}
	return out, true
}

// Reports whether two lists of raw JSON hold the same bytes
func sameRaws(a, b []json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
