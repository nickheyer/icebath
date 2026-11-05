package tools

import (
	"bytes"
	"cmp"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Surrogate code point bounds and the range a surrogateescape decode produces
const (
	surrogateMin       = 0xD800
	surrogateHighMax   = 0xDBFF
	surrogateLowMin    = 0xDC00
	surrogateMax       = 0xDFFF
	surrogateEscapeMin = 0xDC80
	surrogateEscapeMax = 0xDCFF
)

// String or byte slice holding WTF-8 text
type wtf8Text interface{ ~string | ~[]byte }

// Whether the three bytes at i are the generalized UTF-8 form of a surrogate code point
func wtf8SurrogateAt[T wtf8Text](s T, i int) bool {
	return i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF
}

// Code point of the surrogate whose three byte form starts at i
func wtf8SurrogateRune[T wtf8Text](s T, i int) rune {
	return 0xD000 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
}

// Whether r is a UTF-16 surrogate code point
func isSurrogateRune(r rune) bool { return r >= surrogateMin && r <= surrogateMax }

// Leading code point and its byte size, surrogates whole and each stray invalid byte as its surrogateescape char
func wtf8DecodeRune(s string) (rune, int) {
	if len(s) == 0 {
		return utf8.RuneError, 0
	}
	if c := s[0]; c < utf8.RuneSelf {
		return rune(c), 1
	}
	r, size := utf8.DecodeRuneInString(s)
	if r != utf8.RuneError || size > 1 {
		return r, size
	}
	if wtf8SurrogateAt(s, 0) {
		return wtf8SurrogateRune(s, 0), 3
	}
	return surrogateLowMin + rune(s[0]), 1
}

// Trailing code point and its byte size with the same surrogate rules as the forward decode
func wtf8DecodeLastRune(s string) (rune, int) {
	n := len(s)
	if n == 0 {
		return utf8.RuneError, 0
	}
	if c := s[n-1]; c < utf8.RuneSelf {
		return rune(c), 1
	}
	r, size := utf8.DecodeLastRuneInString(s)
	if r != utf8.RuneError || size > 1 {
		return r, size
	}
	if n >= 3 && wtf8SurrogateAt(s, n-3) {
		return wtf8SurrogateRune(s, n-3), 3
	}
	return surrogateLowMin + rune(s[n-1]), 1
}

// Python len of the str s models, one per code point with a surrogate counted once
func wtf8RuneCount(s string) int {
	n := utf8.RuneCountInString(s)
	for i := strings.IndexByte(s, 0xED); i >= 0; {
		if wtf8SurrogateAt(s, i) {
			n -= 2
			i += 3
		} else {
			i++
		}
		j := strings.IndexByte(s[i:], 0xED)
		if j < 0 {
			break
		}
		i += j
	}
	return n
}

// Appends r encoded as generalized UTF-8 so a surrogate keeps its own three bytes
func wtf8AppendRune(p []byte, r rune) []byte {
	if isSurrogateRune(r) {
		return append(p, 0xED, byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
	}
	return utf8.AppendRune(p, r)
}

// Writes r encoded as generalized UTF-8 so a surrogate keeps its own three bytes
func wtf8WriteRune(b *strings.Builder, r rune) {
	if isSurrogateRune(r) {
		b.WriteByte(0xED)
		b.WriteByte(byte(0x80 | (r>>6)&0x3F))
		b.WriteByte(byte(0x80 | r&0x3F))
		return
	}
	b.WriteRune(r)
}

// Code points of s with surrogates and stray bytes kept as their own runes
func wtf8Runes(s string) []rune {
	if utf8.ValidString(s) {
		return []rune(s)
	}
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := wtf8DecodeRune(s[i:])
		out = append(out, r)
		i += size
	}
	return out
}

// String of code points with every surrogate written in its three byte form
func wtf8FromRunes(rs []rune) string {
	buf := make([]byte, 0, len(rs))
	for _, r := range rs {
		buf = wtf8AppendRune(buf, r)
	}
	return string(buf)
}

// Python bytes.decode with utf-8 and surrogateescape, each undecodable byte becoming U+DC80 through U+DCFF
func surrogateEscapeDecode(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out := make([]byte, 0, len(b)+len(b)/2)
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			out = append(out, b[i:i+size]...)
			i += size
			continue
		}
		out = wtf8AppendRune(out, surrogateLowMin+rune(b[i]))
		i++
	}
	return string(out)
}

// UnicodeEncodeError of the utf-8 codec over the surrogate run start to end of a str
type pyUnicodeEncodeError struct {
	char       rune
	start, end int
}

func (e *pyUnicodeEncodeError) Error() string {
	if e.end == e.start+1 {
		return fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", e.char, e.start)
	}
	return fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", e.start, e.end-1)
}

// Python str.encode with utf-8 and surrogateescape, the exact inverse of the surrogateescape decode
func surrogateEscapeEncode(s string) ([]byte, error) {
	if utf8.ValidString(s) {
		return []byte(s), nil
	}
	out := make([]byte, 0, len(s))
	pos := 0
	for i := 0; i < len(s); pos++ {
		r, size := wtf8DecodeRune(s[i:])
		switch {
		case !isSurrogateRune(r):
			out = append(out, s[i:i+size]...)
		case r >= surrogateEscapeMin && r <= surrogateEscapeMax:
			out = append(out, byte(r-surrogateLowMin))
		default:
			end := pos + 1
			for j := i + size; j < len(s); end++ {
				r2, n := wtf8DecodeRune(s[j:])
				if !isSurrogateRune(r2) {
					break
				}
				j += n
			}
			return nil, &pyUnicodeEncodeError{char: r, start: pos, end: end}
		}
		i += size
	}
	return out, nil
}

// Python exception class name the error stands for
func (e *pyUnicodeEncodeError) PyTypeName() string { return "UnicodeEncodeError" }

// Python os.fsencode, surrogateescape on POSIX and unchanged on Windows where Go hands surrogates to UTF-16 APIs
func fsEncode(s string) (string, error) {
	if runtime.GOOS == "windows" || utf8.ValidString(s) {
		return s, nil
	}
	b, err := surrogateEscapeEncode(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Error os.fsencode raises for s, nil when every surrogate names a host byte
func fsEncodeError(s string) error {
	_, err := fsEncode(s)
	return err
}

// Model text naming host bytes, each U+DC80 through U+DCFF its byte and other surrogates left for the OS call to refuse
func fsText(s string) string {
	if runtime.GOOS == "windows" || utf8.ValidString(s) {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		r, size := wtf8DecodeRune(s[i:])
		if r >= surrogateEscapeMin && r <= surrogateEscapeMax {
			out = append(out, byte(r-surrogateLowMin))
		} else {
			out = append(out, s[i:i+size]...)
		}
		i += size
	}
	return string(out)
}

// UnicodeEncodeError of a strict utf-8 encode over the first surrogate run, nil for text holding no surrogate
func strictUTF8EncodeError(s string) error {
	if utf8.ValidString(s) {
		return nil
	}
	pos := 0
	for i := 0; i < len(s); pos++ {
		r, size := wtf8DecodeRune(s[i:])
		if isSurrogateRune(r) {
			end := pos + 1
			for j := i + size; j < len(s); end++ {
				r2, n := wtf8DecodeRune(s[j:])
				if !isSurrogateRune(r2) {
					break
				}
				j += n
			}
			return &pyUnicodeEncodeError{char: r, start: pos, end: end}
		}
		i += size
	}
	return nil
}

// Text a Go JSON server decodes from Python ensure_ascii JSON of s, a high low surrogate pair joined and others U+FFFD
func asciiJSONWireText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := wtf8DecodeRune(s[i:])
		if !isSurrogateRune(r) {
			b.WriteString(s[i : i+size])
			i += size
			continue
		}
		i += size
		if r <= surrogateHighMax && i < len(s) {
			if r2, n := wtf8DecodeRune(s[i:]); r2 >= surrogateLowMin && r2 <= surrogateMax {
				b.WriteRune(utf16.DecodeRune(r, r2))
				i += n
				continue
			}
		}
		b.WriteRune(utf8.RuneError)
	}
	return b.String()
}

// First surrogate outside U+DC80 through U+DCFF, the match of the lone surrogate refusal pattern
func firstUnencodableSurrogate(s string) (rune, bool) {
	if utf8.ValidString(s) {
		return 0, false
	}
	for i := strings.IndexByte(s, 0xED); i >= 0; {
		if wtf8SurrogateAt(s, i) {
			if r := wtf8SurrogateRune(s, i); r < surrogateEscapeMin || r > surrogateEscapeMax {
				return r, true
			}
			i += 3
		} else {
			i++
		}
		j := strings.IndexByte(s[i:], 0xED)
		if j < 0 {
			break
		}
		i += j
	}
	return 0, false
}

// Offset of the first byte that is neither UTF-8 nor a three byte surrogate, ok when there is none
func wtf8ValidIndex(data []byte) (int, bool) {
	for i := 0; i < len(data); {
		if data[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		if wtf8SurrogateAt(data, i) {
			i += 3
			continue
		}
		return i, false
	}
	return len(data), true
}

// UnicodeDecodeError text of a strict utf-8 decode that stops at pos, with the CPython reason and span
func pyUTF8DecodeError(data []byte, pos int) error {
	c := data[pos]
	n, reason := 1, "invalid start byte"
	if c >= 0xC2 && c <= 0xF4 {
		n = utf8MaximalSubpart(data[pos:])
		reason = "invalid continuation byte"
		if pos+n == len(data) {
			reason = "unexpected end of data"
		}
	}
	if n == 1 {
		return fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", c, pos, reason)
	}
	return fmt.Errorf("'utf-8' codec can't decode bytes in position %d-%d: %s", pos, pos+n-1, reason)
}

// Decodes JSON into the encoding/json any shapes with lone surrogate escapes kept as WTF-8 code points
func jsonDecodeWTF8(data []byte) (any, error) { return decodeWTF8JSON(data, false, false) }

// Decodes JSON with objects as insertion ordered dicts and numbers as json.Number text
func jsonDecodeWTF8Ordered(data []byte) (any, error) { return decodeWTF8JSON(data, true, true) }

// Nesting limit encoding/json enforces on arrays and objects
const wtf8MaxJSONDepth = 10000

// Recursive descent JSON reader over validated WTF-8 input
type wtf8JSONDecoder struct {
	data      []byte
	pos       int
	depth     int
	useNumber bool
	ordered   bool
	scratch   []byte
}

// Full decode, a utf-8 decode error for stray bytes and the encoding/json error for any grammar failure
func decodeWTF8JSON(data []byte, useNumber, ordered bool) (any, error) {
	if pos, ok := wtf8ValidIndex(data); !ok {
		return nil, pyUTF8DecodeError(data, pos)
	}
	d := &wtf8JSONDecoder{data: data, useNumber: useNumber, ordered: ordered}
	d.skipSpace()
	v, ok := d.value()
	if ok {
		d.skipSpace()
		ok = d.pos == len(d.data)
	}
	if ok {
		return v, nil
	}
	var err error
	if useNumber {
		err = json.Unmarshal(data, new(json.RawMessage))
	} else {
		var probe any
		err = json.Unmarshal(data, &probe)
	}
	if err == nil {
		err = fmt.Errorf("json: invalid JSON input at offset %d", d.pos)
	}
	return nil, err
}

func (d *wtf8JSONDecoder) skipSpace() {
	for d.pos < len(d.data) {
		switch d.data[d.pos] {
		case ' ', '\t', '\n', '\r':
			d.pos++
		default:
			return
		}
	}
}

func (d *wtf8JSONDecoder) literal(word string) bool {
	if !bytes.HasPrefix(d.data[d.pos:], []byte(word)) {
		return false
	}
	d.pos += len(word)
	return true
}

func (d *wtf8JSONDecoder) value() (any, bool) {
	if d.pos >= len(d.data) {
		return nil, false
	}
	switch c := d.data[d.pos]; {
	case c == '"':
		s, ok := d.str()
		return s, ok
	case c == '{':
		return d.object()
	case c == '[':
		return d.array()
	case c == 't':
		return true, d.literal("true")
	case c == 'f':
		return false, d.literal("false")
	case c == 'n':
		return nil, d.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return d.number()
	}
	return nil, false
}

func (d *wtf8JSONDecoder) digits() int {
	start := d.pos
	for d.pos < len(d.data) && d.data[d.pos] >= '0' && d.data[d.pos] <= '9' {
		d.pos++
	}
	return d.pos - start
}

// Number literal in the RFC 8259 grammar as float64 or json.Number
func (d *wtf8JSONDecoder) number() (any, bool) {
	start := d.pos
	if d.data[d.pos] == '-' {
		d.pos++
	}
	if d.pos >= len(d.data) {
		return nil, false
	}
	if d.data[d.pos] == '0' {
		d.pos++
	} else if d.digits() == 0 {
		return nil, false
	}
	if d.pos < len(d.data) && d.data[d.pos] == '.' {
		d.pos++
		if d.digits() == 0 {
			return nil, false
		}
	}
	if d.pos < len(d.data) && (d.data[d.pos] == 'e' || d.data[d.pos] == 'E') {
		d.pos++
		if d.pos < len(d.data) && (d.data[d.pos] == '+' || d.data[d.pos] == '-') {
			d.pos++
		}
		if d.digits() == 0 {
			return nil, false
		}
	}
	text := string(d.data[start:d.pos])
	if d.useNumber {
		return json.Number(text), true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, false
	}
	return f, true
}

// Value of four hex digits at i, negative when any is not hex
func jsonHex4(data []byte, i int) rune {
	if i+4 > len(data) {
		return -1
	}
	var r rune
	for _, c := range data[i : i+4] {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			c = c - 'A' + 10
		default:
			return -1
		}
		r = r<<4 | rune(c)
	}
	return r
}

// String literal with escapes resolved, a valid surrogate escape pair joined and a lone one kept as WTF-8
func (d *wtf8JSONDecoder) str() (string, bool) {
	d.pos++
	start := d.pos
	for d.pos < len(d.data) {
		c := d.data[d.pos]
		if c == '"' {
			s := string(d.data[start:d.pos])
			d.pos++
			return s, true
		}
		if c == '\\' || c < 0x20 {
			break
		}
		d.pos++
	}
	buf := append(d.scratch[:0], d.data[start:d.pos]...)
	for d.pos < len(d.data) {
		c := d.data[d.pos]
		switch {
		case c == '"':
			d.pos++
			d.scratch = buf
			return string(buf), true
		case c < 0x20:
			return "", false
		case c != '\\':
			buf = append(buf, c)
			d.pos++
			continue
		}
		if d.pos+1 >= len(d.data) {
			return "", false
		}
		esc := d.data[d.pos+1]
		d.pos += 2
		switch esc {
		case '"', '\\', '/':
			buf = append(buf, esc)
		case 'b':
			buf = append(buf, '\b')
		case 'f':
			buf = append(buf, '\f')
		case 'n':
			buf = append(buf, '\n')
		case 'r':
			buf = append(buf, '\r')
		case 't':
			buf = append(buf, '\t')
		case 'u':
			r := jsonHex4(d.data, d.pos)
			if r < 0 {
				return "", false
			}
			d.pos += 4
			if r >= surrogateMin && r <= surrogateHighMax && d.pos+6 <= len(d.data) && d.data[d.pos] == '\\' && d.data[d.pos+1] == 'u' {
				if r2 := jsonHex4(d.data, d.pos+2); r2 >= surrogateLowMin && r2 <= surrogateMax {
					r = 0x10000 + (r-surrogateMin)<<10 + (r2 - surrogateLowMin)
					d.pos += 6
				}
			}
			buf = wtf8AppendRune(buf, r)
		default:
			return "", false
		}
	}
	return "", false
}

func (d *wtf8JSONDecoder) object() (any, bool) {
	if d.depth++; d.depth > wtf8MaxJSONDepth {
		return nil, false
	}
	d.pos++
	var plain map[string]any
	var ordered *pyJSONObject
	if d.ordered {
		ordered = &pyJSONObject{vals: map[string]any{}}
	} else {
		plain = map[string]any{}
	}
	d.skipSpace()
	if d.pos < len(d.data) && d.data[d.pos] == '}' {
		d.pos++
	} else {
		for {
			if d.pos >= len(d.data) || d.data[d.pos] != '"' {
				return nil, false
			}
			key, ok := d.str()
			if !ok {
				return nil, false
			}
			d.skipSpace()
			if d.pos >= len(d.data) || d.data[d.pos] != ':' {
				return nil, false
			}
			d.pos++
			d.skipSpace()
			val, ok := d.value()
			if !ok {
				return nil, false
			}
			if d.ordered {
				ordered.set(key, val)
			} else {
				plain[key] = val
			}
			d.skipSpace()
			if d.pos >= len(d.data) {
				return nil, false
			}
			if d.data[d.pos] == '}' {
				d.pos++
				break
			}
			if d.data[d.pos] != ',' {
				return nil, false
			}
			d.pos++
			d.skipSpace()
		}
	}
	d.depth--
	if d.ordered {
		return ordered, true
	}
	return plain, true
}

func (d *wtf8JSONDecoder) array() (any, bool) {
	if d.depth++; d.depth > wtf8MaxJSONDepth {
		return nil, false
	}
	d.pos++
	out := []any{}
	d.skipSpace()
	if d.pos < len(d.data) && d.data[d.pos] == ']' {
		d.pos++
		d.depth--
		return out, true
	}
	for {
		val, ok := d.value()
		if !ok {
			return nil, false
		}
		out = append(out, val)
		d.skipSpace()
		if d.pos >= len(d.data) {
			return nil, false
		}
		if d.data[d.pos] == ']' {
			d.pos++
			d.depth--
			return out, true
		}
		if d.data[d.pos] != ',' {
			return nil, false
		}
		d.pos++
		d.skipSpace()
	}
}

// Encodes like encoding/json without HTML escaping, surrogates written as code points like json.dumps with ensure_ascii false
func jsonEncodeWTF8(v any) ([]byte, error) {
	e := wtf8EncodePool.Get().(*wtf8EncodeState)
	defer wtf8EncodePool.Put(e)
	if err := e.marshal(v); err != nil {
		return nil, err
	}
	return append([]byte(nil), e.buf...), nil
}

// Encodes like jsonEncodeWTF8 with every non ASCII character escaped the way json.dumps with ensure_ascii writes it
func jsonEncodeWTF8ASCII(v any) ([]byte, error) {
	out, err := jsonEncodeWTF8(v)
	if err != nil {
		return nil, err
	}
	return escapeJSONNonASCII(out), nil
}

// Rewrites each non ASCII character of encoded JSON as backslash u escapes, astral ones as a surrogate pair
func escapeJSONNonASCII(out []byte) []byte {
	const hex = "0123456789abcdef"
	res := make([]byte, 0, len(out))
	hex4 := func(u rune) {
		res = append(res, '\\', 'u', hex[u>>12&0xF], hex[u>>8&0xF], hex[u>>4&0xF], hex[u&0xF])
	}
	for i := 0; i < len(out); {
		if out[i] < utf8.RuneSelf {
			res = append(res, out[i])
			i++
			continue
		}
		r, size := wtf8DecodeRune(string(out[i:min(len(out), i+utf8.UTFMax)]))
		if r > 0xFFFF {
			r -= 0x10000
			hex4(surrogateMin + r>>10)
			hex4(surrogateLowMin + r&0x3FF)
		} else {
			hex4(r)
		}
		i += size
	}
	return res
}

// Indented form of jsonEncodeWTF8 with the encoding/json Encoder indent layout
func jsonEncodeWTF8Indent(v any, prefix, indent string) ([]byte, error) {
	compact, err := jsonEncodeWTF8(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, prefix, indent); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Quoted JSON string of WTF-8 text, surrogates kept as their code points and stray bytes as surrogateescape chars
func appendJSONStringWTF8(dst []byte, src string) []byte {
	const hex = "0123456789abcdef"
	const ones, highs = 0x0101010101010101, 0x8080808080808080
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(src); {
		for i+8 <= len(src) {
			x := uint64(src[i]) | uint64(src[i+1])<<8 | uint64(src[i+2])<<16 | uint64(src[i+3])<<24 |
				uint64(src[i+4])<<32 | uint64(src[i+5])<<40 | uint64(src[i+6])<<48 | uint64(src[i+7])<<56
			q, b := x^0x2222222222222222, x^0x5c5c5c5c5c5c5c5c
			if m := (x | (x - 0x2020202020202020) | ((q - ones) &^ q) | ((b - ones) &^ b)) & highs; m != 0 {
				i += bits.TrailingZeros64(m) >> 3
				break
			}
			i += 8
		}
		if i >= len(src) {
			break
		}
		c := src[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			dst = append(dst, src[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(src[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		if wtf8SurrogateAt(src, i) {
			i += 3
			continue
		}
		dst = append(dst, src[start:i]...)
		dst = wtf8AppendRune(dst, surrogateLowMin+rune(c))
		i++
		start = i
	}
	dst = append(dst, src[start:]...)
	return append(dst, '"')
}

// Reflection encoder mirroring encoding/json byte for byte apart from surrogates, stray bytes, line separators and HTML escaping
type wtf8EncodeState struct {
	buf      []byte
	ptrLevel uint
	ptrSeen  map[any]struct{}
}

// Pointer depth past which the encoder starts checking for cycles, as encoding/json does
const wtf8CycleDepth = 1000

// Reusable encoder states so a call allocates only its result
var wtf8EncodePool = sync.Pool{New: func() any { return &wtf8EncodeState{} }}

type wtf8EncodeError struct{ err error }

type wtf8EncoderFunc func(e *wtf8EncodeState, v reflect.Value, quoted bool)

// MarshalerError text for a failing MarshalText, which json.MarshalerError cannot name
type wtf8MarshalTextError struct {
	typ reflect.Type
	err error
}

func (e *wtf8MarshalTextError) Error() string {
	return "json: error calling MarshalText for type " + e.typ.String() + ": " + e.err.Error()
}

func (e *wtf8MarshalTextError) Unwrap() error { return e.err }

func (e *wtf8EncodeState) marshal(v any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			je, ok := r.(wtf8EncodeError)
			if !ok {
				panic(r)
			}
			err = je.err
		}
	}()
	e.buf, e.ptrLevel = e.buf[:0], 0
	clear(e.ptrSeen)
	e.encodeAny(v)
	return nil
}

// Common dynamic values written without reflection, everything else through the type encoders
func (e *wtf8EncodeState) encodeAny(v any) {
	switch x := v.(type) {
	case nil:
		e.buf = append(e.buf, "null"...)
	case string:
		e.buf = appendJSONStringWTF8(e.buf, x)
	case bool:
		e.buf = strconv.AppendBool(e.buf, x)
	case int:
		e.buf = strconv.AppendInt(e.buf, int64(x), 10)
	case int64:
		e.buf = strconv.AppendInt(e.buf, x, 10)
	case map[string]any:
		if x == nil {
			e.buf = append(e.buf, "null"...)
			return
		}
		key, tracked := e.enterPointer(func() any { return reflect.ValueOf(v).UnsafePointer() }, reflect.ValueOf(v))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		e.buf = append(e.buf, '{')
		for i, k := range keys {
			if i > 0 {
				e.buf = append(e.buf, ',')
			}
			e.buf = appendJSONStringWTF8(e.buf, k)
			e.buf = append(e.buf, ':')
			e.encodeAny(x[k])
		}
		e.buf = append(e.buf, '}')
		e.leavePointer(key, tracked)
	case []any:
		if x == nil {
			e.buf = append(e.buf, "null"...)
			return
		}
		key, tracked := e.enterPointer(func() any {
			return struct {
				ptr any
				len int
			}{reflect.ValueOf(v).UnsafePointer(), len(x)}
		}, reflect.ValueOf(v))
		e.buf = append(e.buf, '[')
		for i, item := range x {
			if i > 0 {
				e.buf = append(e.buf, ',')
			}
			e.encodeAny(item)
		}
		e.buf = append(e.buf, ']')
		e.leavePointer(key, tracked)
	default:
		e.reflectValue(reflect.ValueOf(v), false)
	}
}

func (e *wtf8EncodeState) fail(err error) { panic(wtf8EncodeError{err}) }

func (e *wtf8EncodeState) reflectValue(v reflect.Value, quoted bool) {
	if !v.IsValid() {
		e.buf = append(e.buf, "null"...)
		return
	}
	wtf8TypeEncoder(v.Type())(e, v, quoted)
}

var (
	wtf8EncoderCache     sync.Map
	wtf8FieldCache       sync.Map
	wtf8MarshalerType    = reflect.TypeFor[json.Marshaler]()
	wtf8TextMarshalType  = reflect.TypeFor[encoding.TextMarshaler]()
	wtf8NumberType       = reflect.TypeFor[json.Number]()
	wtf8IsZeroerType     = reflect.TypeFor[interface{ IsZero() bool }]()
	wtf8TagPunctuation   = "!#$%&()*+-./:;<=>?@[]^_{|}~ "
	errWTF8UnexpectedKey = errors.New("unexpected map key type")
)

// Cached encoder for a type, recursive types resolved through an indirect entry
func wtf8TypeEncoder(t reflect.Type) wtf8EncoderFunc {
	if fi, ok := wtf8EncoderCache.Load(t); ok {
		return fi.(wtf8EncoderFunc)
	}
	indirect := sync.OnceValue(func() wtf8EncoderFunc { return newWTF8TypeEncoder(t, true) })
	fi, loaded := wtf8EncoderCache.LoadOrStore(t, wtf8EncoderFunc(func(e *wtf8EncodeState, v reflect.Value, quoted bool) {
		indirect()(e, v, quoted)
	}))
	if loaded {
		return fi.(wtf8EncoderFunc)
	}
	f := indirect()
	wtf8EncoderCache.Store(t, f)
	return f
}

func newWTF8TypeEncoder(t reflect.Type, allowAddr bool) wtf8EncoderFunc {
	if t.Kind() != reflect.Pointer && allowAddr && reflect.PointerTo(t).Implements(wtf8MarshalerType) {
		return wtf8CondAddr(wtf8AddrMarshalerEncoder, newWTF8TypeEncoder(t, false))
	}
	if t.Implements(wtf8MarshalerType) {
		return wtf8MarshalerEncoder
	}
	if t.Kind() != reflect.Pointer && allowAddr && reflect.PointerTo(t).Implements(wtf8TextMarshalType) {
		return wtf8CondAddr(wtf8AddrTextMarshalerEncoder, newWTF8TypeEncoder(t, false))
	}
	if t.Implements(wtf8TextMarshalType) {
		return wtf8TextMarshalerEncoder
	}
	switch t.Kind() {
	case reflect.Bool:
		return wtf8BoolEncoder
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return wtf8IntEncoder
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return wtf8UintEncoder
	case reflect.Float32:
		return wtf8FloatEncoder(32)
	case reflect.Float64:
		return wtf8FloatEncoder(64)
	case reflect.String:
		return wtf8StringEncoder
	case reflect.Interface:
		return wtf8InterfaceEncoder
	case reflect.Struct:
		return newWTF8StructEncoder(t)
	case reflect.Map:
		return newWTF8MapEncoder(t)
	case reflect.Slice:
		return newWTF8SliceEncoder(t)
	case reflect.Array:
		return newWTF8ArrayEncoder(t)
	case reflect.Pointer:
		return newWTF8PtrEncoder(t)
	}
	return wtf8UnsupportedEncoder
}

func wtf8CondAddr(canAddr, otherwise wtf8EncoderFunc) wtf8EncoderFunc {
	return func(e *wtf8EncodeState, v reflect.Value, quoted bool) {
		if v.CanAddr() {
			canAddr(e, v, quoted)
		} else {
			otherwise(e, v, quoted)
		}
	}
}

// Appends MarshalJSON output compacted without HTML escaping, surrogate bytes passing through untouched
func (e *wtf8EncodeState) appendMarshaled(t reflect.Type, m json.Marshaler) {
	b, err := m.MarshalJSON()
	if err == nil {
		var compact bytes.Buffer
		if err = json.Compact(&compact, b); err == nil {
			e.buf = append(e.buf, compact.Bytes()...)
			return
		}
	}
	e.fail(&json.MarshalerError{Type: t, Err: err})
}

func wtf8MarshalerEncoder(e *wtf8EncodeState, v reflect.Value, _ bool) {
	if v.Kind() == reflect.Pointer && v.IsNil() {
		e.buf = append(e.buf, "null"...)
		return
	}
	m, ok := reflect.TypeAssert[json.Marshaler](v)
	if !ok {
		e.buf = append(e.buf, "null"...)
		return
	}
	e.appendMarshaled(v.Type(), m)
}

func wtf8AddrMarshalerEncoder(e *wtf8EncodeState, v reflect.Value, _ bool) {
	va := v.Addr()
	if va.IsNil() {
		e.buf = append(e.buf, "null"...)
		return
	}
	m, _ := reflect.TypeAssert[json.Marshaler](va)
	e.appendMarshaled(v.Type(), m)
}

func (e *wtf8EncodeState) appendText(t reflect.Type, m encoding.TextMarshaler) {
	b, err := m.MarshalText()
	if err != nil {
		e.fail(&wtf8MarshalTextError{typ: t, err: err})
	}
	e.buf = appendJSONStringWTF8(e.buf, string(b))
}

func wtf8TextMarshalerEncoder(e *wtf8EncodeState, v reflect.Value, _ bool) {
	if v.Kind() == reflect.Pointer && v.IsNil() {
		e.buf = append(e.buf, "null"...)
		return
	}
	m, ok := reflect.TypeAssert[encoding.TextMarshaler](v)
	if !ok {
		e.buf = append(e.buf, "null"...)
		return
	}
	e.appendText(v.Type(), m)
}

func wtf8AddrTextMarshalerEncoder(e *wtf8EncodeState, v reflect.Value, _ bool) {
	va := v.Addr()
	if va.IsNil() {
		e.buf = append(e.buf, "null"...)
		return
	}
	m, _ := reflect.TypeAssert[encoding.TextMarshaler](va)
	e.appendText(v.Type(), m)
}

func (e *wtf8EncodeState) quote(quoted bool) {
	if quoted {
		e.buf = append(e.buf, '"')
	}
}

func wtf8BoolEncoder(e *wtf8EncodeState, v reflect.Value, quoted bool) {
	e.quote(quoted)
	e.buf = strconv.AppendBool(e.buf, v.Bool())
	e.quote(quoted)
}

func wtf8IntEncoder(e *wtf8EncodeState, v reflect.Value, quoted bool) {
	e.quote(quoted)
	e.buf = strconv.AppendInt(e.buf, v.Int(), 10)
	e.quote(quoted)
}

func wtf8UintEncoder(e *wtf8EncodeState, v reflect.Value, quoted bool) {
	e.quote(quoted)
	e.buf = strconv.AppendUint(e.buf, v.Uint(), 10)
	e.quote(quoted)
}

// Float in the ES6 number to string form encoding/json writes, refusing NaN and infinities
func wtf8FloatEncoder(bits int) wtf8EncoderFunc {
	return func(e *wtf8EncodeState, v reflect.Value, quoted bool) {
		f := v.Float()
		if math.IsInf(f, 0) || math.IsNaN(f) {
			e.fail(&json.UnsupportedValueError{Value: v, Str: strconv.FormatFloat(f, 'g', -1, bits)})
		}
		e.quote(quoted)
		abs := math.Abs(f)
		format := byte('f')
		if abs != 0 && (bits == 64 && (abs < 1e-6 || abs >= 1e21) || bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21)) {
			format = 'e'
		}
		e.buf = strconv.AppendFloat(e.buf, f, format, -1, bits)
		if format == 'e' {
			n := len(e.buf)
			if n >= 4 && e.buf[n-4] == 'e' && e.buf[n-3] == '-' && e.buf[n-2] == '0' {
				e.buf[n-2] = e.buf[n-1]
				e.buf = e.buf[:n-1]
			}
		}
		e.quote(quoted)
	}
}

func wtf8StringEncoder(e *wtf8EncodeState, v reflect.Value, quoted bool) {
	if v.Type() == wtf8NumberType {
		numStr := v.String()
		if numStr == "" {
			numStr = "0"
		}
		if !wtf8ValidNumber(numStr) {
			e.fail(fmt.Errorf("json: invalid number literal %q", numStr))
		}
		e.quote(quoted)
		e.buf = append(e.buf, numStr...)
		e.quote(quoted)
		return
	}
	if quoted {
		e.buf = appendJSONStringWTF8(e.buf, string(appendJSONStringWTF8(nil, v.String())))
		return
	}
	e.buf = appendJSONStringWTF8(e.buf, v.String())
}

// Whether s is a JSON number literal
func wtf8ValidNumber(s string) bool {
	d := &wtf8JSONDecoder{data: []byte(s), useNumber: true}
	_, ok := d.number()
	return ok && d.pos == len(s)
}

func wtf8InterfaceEncoder(e *wtf8EncodeState, v reflect.Value, _ bool) {
	if v.IsNil() {
		e.buf = append(e.buf, "null"...)
		return
	}
	e.reflectValue(v.Elem(), false)
}

func wtf8UnsupportedEncoder(e *wtf8EncodeState, v reflect.Value, _ bool) {
	e.fail(&json.UnsupportedTypeError{Type: v.Type()})
}

// One encodable struct field with its tag options and index path
type wtf8Field struct {
	name      string
	tag       bool
	index     []int
	typ       reflect.Type
	omitEmpty bool
	omitZero  bool
	isZero    func(reflect.Value) bool
	quoted    bool
	encoder   wtf8EncoderFunc
}

func wtf8IsEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}

func newWTF8StructEncoder(t reflect.Type) wtf8EncoderFunc {
	return func(e *wtf8EncodeState, v reflect.Value, _ bool) {
		fields := wtf8CachedFields(t)
		next := byte('{')
	FieldLoop:
		for i := range fields {
			f := &fields[i]
			fv := v
			for _, idx := range f.index {
				if fv.Kind() == reflect.Pointer {
					if fv.IsNil() {
						continue FieldLoop
					}
					fv = fv.Elem()
				}
				fv = fv.Field(idx)
			}
			if (f.omitEmpty && wtf8IsEmptyValue(fv)) ||
				(f.omitZero && (f.isZero == nil && fv.IsZero() || f.isZero != nil && f.isZero(fv))) {
				continue
			}
			e.buf = append(e.buf, next)
			next = ','
			e.buf = appendJSONStringWTF8(e.buf, f.name)
			e.buf = append(e.buf, ':')
			f.encoder(e, fv, f.quoted)
		}
		if next == '{' {
			e.buf = append(e.buf, "{}"...)
		} else {
			e.buf = append(e.buf, '}')
		}
	}
}

func wtf8CachedFields(t reflect.Type) []wtf8Field {
	if f, ok := wtf8FieldCache.Load(t); ok {
		return f.([]wtf8Field)
	}
	f, _ := wtf8FieldCache.LoadOrStore(t, wtf8TypeFields(t))
	return f.([]wtf8Field)
}

func wtf8ValidTag(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(wtf8TagPunctuation, c) && !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return false
		}
	}
	return true
}

func wtf8TagHas(opts, name string) bool {
	for opts != "" {
		var opt string
		opt, opts, _ = strings.Cut(opts, ",")
		if opt == name {
			return true
		}
	}
	return false
}

func wtf8TypeByIndex(t reflect.Type, index []int) reflect.Type {
	for _, i := range index {
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		t = t.Field(i).Type
	}
	return t
}

// IsZero method probe for an omitzero field, nil when the type has none
func wtf8ZeroFunc(t reflect.Type) func(reflect.Value) bool {
	zeroer := func(v reflect.Value) bool { return v.Interface().(interface{ IsZero() bool }).IsZero() }
	switch {
	case t.Kind() == reflect.Interface && t.Implements(wtf8IsZeroerType):
		return func(v reflect.Value) bool {
			return v.IsNil() || (v.Elem().Kind() == reflect.Pointer && v.Elem().IsNil()) || zeroer(v)
		}
	case t.Kind() == reflect.Pointer && t.Implements(wtf8IsZeroerType):
		return func(v reflect.Value) bool { return v.IsNil() || zeroer(v) }
	case t.Implements(wtf8IsZeroerType):
		return zeroer
	case reflect.PointerTo(t).Implements(wtf8IsZeroerType):
		return func(v reflect.Value) bool {
			if !v.CanAddr() {
				boxed := reflect.New(v.Type()).Elem()
				boxed.Set(v)
				v = boxed
			}
			return zeroer(v.Addr())
		}
	}
	return nil
}

// Visible fields of a struct type under the Go embedding rules with json tag promotion
func wtf8TypeFields(t reflect.Type) []wtf8Field {
	current := []wtf8Field{}
	next := []wtf8Field{{typ: t}}
	var count, nextCount map[reflect.Type]int
	visited := map[reflect.Type]bool{}
	var fields []wtf8Field
	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}
		for _, f := range current {
			if visited[f.typ] {
				continue
			}
			visited[f.typ] = true
			for i := 0; i < f.typ.NumField(); i++ {
				sf := f.typ.Field(i)
				if sf.Anonymous {
					st := sf.Type
					if st.Kind() == reflect.Pointer {
						st = st.Elem()
					}
					if !sf.IsExported() && st.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				if !wtf8ValidTag(name) {
					name = ""
				}
				index := make([]int, len(f.index)+1)
				copy(index, f.index)
				index[len(f.index)] = i
				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				quoted := false
				if wtf8TagHas(opts, "string") {
					switch ft.Kind() {
					case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
						reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
						reflect.Float32, reflect.Float64, reflect.String:
						quoted = true
					}
				}
				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					tagged := name != ""
					if name == "" {
						name = sf.Name
					}
					fld := wtf8Field{name: name, tag: tagged, index: index, typ: ft,
						omitEmpty: wtf8TagHas(opts, "omitempty"), omitZero: wtf8TagHas(opts, "omitzero"), quoted: quoted}
					if fld.omitZero {
						fld.isZero = wtf8ZeroFunc(sf.Type)
					}
					fields = append(fields, fld)
					if count[f.typ] > 1 {
						fields = append(fields, fields[len(fields)-1])
					}
					continue
				}
				nextCount[ft]++
				if nextCount[ft] == 1 {
					next = append(next, wtf8Field{name: ft.Name(), index: index, typ: ft})
				}
			}
		}
	}
	slices.SortFunc(fields, func(a, b wtf8Field) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		if c := cmp.Compare(len(a.index), len(b.index)); c != 0 {
			return c
		}
		if a.tag != b.tag {
			if a.tag {
				return -1
			}
			return 1
		}
		return slices.Compare(a.index, b.index)
	})
	out := fields[:0]
	for advance, i := 0, 0; i < len(fields); i += advance {
		name := fields[i].name
		for advance = 1; i+advance < len(fields) && fields[i+advance].name == name; advance++ {
		}
		group := fields[i : i+advance]
		if len(group) > 1 && len(group[0].index) == len(group[1].index) && group[0].tag == group[1].tag {
			continue
		}
		out = append(out, group[0])
	}
	fields = out
	slices.SortFunc(fields, func(a, b wtf8Field) int { return slices.Compare(a.index, b.index) })
	for i := range fields {
		fields[i].encoder = wtf8TypeEncoder(wtf8TypeByIndex(t, fields[i].index))
	}
	return fields
}

// Cycle guard entered once nesting passes the depth encoding/json starts checking at, the key built only then
func (e *wtf8EncodeState) enterPointer(keyOf func() any, v reflect.Value) (any, bool) {
	if e.ptrLevel++; e.ptrLevel <= wtf8CycleDepth {
		return nil, false
	}
	key := keyOf()
	if e.ptrSeen == nil {
		e.ptrSeen = map[any]struct{}{}
	}
	if _, ok := e.ptrSeen[key]; ok {
		e.fail(&json.UnsupportedValueError{Value: v, Str: fmt.Sprintf("encountered a cycle via %s", v.Type())})
	}
	e.ptrSeen[key] = struct{}{}
	return key, true
}

// Leaves the nesting level enterPointer entered, forgetting the tracked key
func (e *wtf8EncodeState) leavePointer(key any, tracked bool) {
	if tracked {
		delete(e.ptrSeen, key)
	}
	e.ptrLevel--
}

func newWTF8MapEncoder(t reflect.Type) wtf8EncoderFunc {
	switch t.Key().Kind() {
	case reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
	default:
		if !t.Key().Implements(wtf8TextMarshalType) {
			return wtf8UnsupportedEncoder
		}
	}
	elemEnc := wtf8TypeEncoder(t.Elem())
	return func(e *wtf8EncodeState, v reflect.Value, _ bool) {
		if v.IsNil() {
			e.buf = append(e.buf, "null"...)
			return
		}
		key, tracked := e.enterPointer(func() any { return v.UnsafePointer() }, v)
		type entry struct {
			key string
			val reflect.Value
		}
		entries := make([]entry, 0, v.Len())
		for it := v.MapRange(); it.Next(); {
			key, err := wtf8MapKey(it.Key())
			if err != nil {
				e.fail(fmt.Errorf("json: encoding error for type %q: %q", v.Type().String(), err.Error()))
			}
			entries = append(entries, entry{key, it.Value()})
		}
		slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })
		e.buf = append(e.buf, '{')
		for i, kv := range entries {
			if i > 0 {
				e.buf = append(e.buf, ',')
			}
			e.buf = appendJSONStringWTF8(e.buf, kv.key)
			e.buf = append(e.buf, ':')
			elemEnc(e, kv.val, false)
		}
		e.buf = append(e.buf, '}')
		e.leavePointer(key, tracked)
	}
}

func wtf8MapKey(k reflect.Value) (string, error) {
	if k.Kind() == reflect.String {
		return k.String(), nil
	}
	if tm, ok := reflect.TypeAssert[encoding.TextMarshaler](k); ok {
		if k.Kind() == reflect.Pointer && k.IsNil() {
			return "", nil
		}
		buf, err := tm.MarshalText()
		return string(buf), err
	}
	switch k.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(k.Uint(), 10), nil
	}
	return "", errWTF8UnexpectedKey
}

func newWTF8SliceEncoder(t reflect.Type) wtf8EncoderFunc {
	if t.Elem().Kind() == reflect.Uint8 {
		p := reflect.PointerTo(t.Elem())
		if !p.Implements(wtf8MarshalerType) && !p.Implements(wtf8TextMarshalType) {
			return func(e *wtf8EncodeState, v reflect.Value, _ bool) {
				if v.IsNil() {
					e.buf = append(e.buf, "null"...)
					return
				}
				e.buf = append(e.buf, '"')
				e.buf = base64.StdEncoding.AppendEncode(e.buf, v.Bytes())
				e.buf = append(e.buf, '"')
			}
		}
	}
	arrayEnc := newWTF8ArrayEncoder(t)
	return func(e *wtf8EncodeState, v reflect.Value, quoted bool) {
		if v.IsNil() {
			e.buf = append(e.buf, "null"...)
			return
		}
		key, tracked := e.enterPointer(func() any {
			return struct {
				ptr any
				len int
			}{v.UnsafePointer(), v.Len()}
		}, v)
		arrayEnc(e, v, quoted)
		e.leavePointer(key, tracked)
	}
}

func newWTF8ArrayEncoder(t reflect.Type) wtf8EncoderFunc {
	elemEnc := wtf8TypeEncoder(t.Elem())
	return func(e *wtf8EncodeState, v reflect.Value, _ bool) {
		e.buf = append(e.buf, '[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				e.buf = append(e.buf, ',')
			}
			elemEnc(e, v.Index(i), false)
		}
		e.buf = append(e.buf, ']')
	}
}

func newWTF8PtrEncoder(t reflect.Type) wtf8EncoderFunc {
	elemEnc := wtf8TypeEncoder(t.Elem())
	return func(e *wtf8EncodeState, v reflect.Value, quoted bool) {
		if v.IsNil() {
			e.buf = append(e.buf, "null"...)
			return
		}
		key, tracked := e.enterPointer(v.Interface, v)
		elemEnc(e, v.Elem(), quoted)
		e.leavePointer(key, tracked)
	}
}
