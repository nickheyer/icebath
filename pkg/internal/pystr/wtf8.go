package pystr

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Surrogate code point bounds and the range a surrogateescape decode produces
const (
	SurrogateMin       = 0xD800
	SurrogateHighMax   = 0xDBFF
	SurrogateLowMin    = 0xDC00
	SurrogateMax       = 0xDFFF
	SurrogateEscapeMin = 0xDC80
	SurrogateEscapeMax = 0xDCFF
)

// String or byte slice holding WTF-8 text
type WTF8Text interface{ ~string | ~[]byte }

// Whether the three bytes at i are the generalized UTF-8 form of a surrogate code point
func SurrogateAt[T WTF8Text](s T, i int) bool {
	return i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF
}

// Code point of the surrogate whose three byte form starts at i
func SurrogateRune[T WTF8Text](s T, i int) rune {
	return 0xD000 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
}

// Whether r is a UTF-16 surrogate code point
func IsSurrogate(r rune) bool { return r >= SurrogateMin && r <= SurrogateMax }

// Leading code point and its byte size, surrogates whole and each stray invalid byte as its surrogateescape char
func DecodeRune(s string) (rune, int) {
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
	if SurrogateAt(s, 0) {
		return SurrogateRune(s, 0), 3
	}
	return SurrogateLowMin + rune(s[0]), 1
}

// Trailing code point and its byte size with the same surrogate rules as the forward decode
func DecodeLastRune(s string) (rune, int) {
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
	if n >= 3 && SurrogateAt(s, n-3) {
		return SurrogateRune(s, n-3), 3
	}
	return SurrogateLowMin + rune(s[n-1]), 1
}

// Python len of the str s models, one per code point with a surrogate counted once
func Len(s string) int {
	n := utf8.RuneCountInString(s)
	for i := strings.IndexByte(s, 0xED); i >= 0; {
		if SurrogateAt(s, i) {
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
func AppendRune(p []byte, r rune) []byte {
	if IsSurrogate(r) {
		return append(p, 0xED, byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
	}
	return utf8.AppendRune(p, r)
}

// Writes r encoded as generalized UTF-8 so a surrogate keeps its own three bytes
func WriteRune(b *strings.Builder, r rune) {
	if IsSurrogate(r) {
		b.WriteByte(0xED)
		b.WriteByte(byte(0x80 | (r>>6)&0x3F))
		b.WriteByte(byte(0x80 | r&0x3F))
		return
	}
	b.WriteRune(r)
}

// Code points of s with surrogates and stray bytes kept as their own runes
func Runes(s string) []rune {
	if utf8.ValidString(s) {
		return []rune(s)
	}
	out := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := DecodeRune(s[i:])
		out = append(out, r)
		i += size
	}
	return out
}

// String of code points with every surrogate written in its three byte form
func FromRunes(rs []rune) string {
	buf := make([]byte, 0, len(rs))
	for _, r := range rs {
		buf = AppendRune(buf, r)
	}
	return string(buf)
}

// Python bytes.decode with utf-8 and surrogateescape, each undecodable byte becoming U+DC80 through U+DCFF
func SurrogateEscapeDecode(b []byte) string {
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
		out = AppendRune(out, SurrogateLowMin+rune(b[i]))
		i++
	}
	return string(out)
}

// UnicodeEncodeError of the utf-8 codec over the surrogate run start to end of a str
type UnicodeEncodeError struct {
	char       rune
	start, end int
}

func (e *UnicodeEncodeError) Error() string {
	if e.end == e.start+1 {
		return fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", e.char, e.start)
	}
	return fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", e.start, e.end-1)
}

// Python str.encode with utf-8 and surrogateescape, the exact inverse of the surrogateescape decode
func SurrogateEscapeEncode(s string) ([]byte, error) {
	if utf8.ValidString(s) {
		return []byte(s), nil
	}
	out := make([]byte, 0, len(s))
	pos := 0
	for i := 0; i < len(s); pos++ {
		r, size := DecodeRune(s[i:])
		switch {
		case !IsSurrogate(r):
			out = append(out, s[i:i+size]...)
		case r >= SurrogateEscapeMin && r <= SurrogateEscapeMax:
			out = append(out, byte(r-SurrogateLowMin))
		default:
			end := pos + 1
			for j := i + size; j < len(s); end++ {
				r2, n := DecodeRune(s[j:])
				if !IsSurrogate(r2) {
					break
				}
				j += n
			}
			return nil, &UnicodeEncodeError{char: r, start: pos, end: end}
		}
		i += size
	}
	return out, nil
}

// Python exception class name the error stands for
func (e *UnicodeEncodeError) PyTypeName() string { return "UnicodeEncodeError" }

// UnicodeEncodeError of a strict utf-8 encode over the first surrogate run, nil for text holding no surrogate
func StrictUTF8EncodeError(s string) error {
	if utf8.ValidString(s) {
		return nil
	}
	pos := 0
	for i := 0; i < len(s); pos++ {
		r, size := DecodeRune(s[i:])
		if IsSurrogate(r) {
			end := pos + 1
			for j := i + size; j < len(s); end++ {
				r2, n := DecodeRune(s[j:])
				if !IsSurrogate(r2) {
					break
				}
				j += n
			}
			return &UnicodeEncodeError{char: r, start: pos, end: end}
		}
		i += size
	}
	return nil
}

// First surrogate outside U+DC80 through U+DCFF, the match of the lone surrogate refusal pattern
func FirstUnencodableSurrogate(s string) (rune, bool) {
	if utf8.ValidString(s) {
		return 0, false
	}
	for i := strings.IndexByte(s, 0xED); i >= 0; {
		if SurrogateAt(s, i) {
			if r := SurrogateRune(s, i); r < SurrogateEscapeMin || r > SurrogateEscapeMax {
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
func ValidIndex(data []byte) (int, bool) {
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
		if SurrogateAt(data, i) {
			i += 3
			continue
		}
		return i, false
	}
	return len(data), true
}

// UnicodeDecodeError text of a strict utf-8 decode that stops at pos, with the CPython reason and span
func UTF8DecodeError(data []byte, pos int) error {
	c := data[pos]
	n, reason := 1, "invalid start byte"
	if c >= 0xC2 && c <= 0xF4 {
		n = UTF8MaximalSubpart(data[pos:])
		reason = "invalid continuation byte"
		if pos+n == len(data) {
			reason = "unexpected end of data"
		}
	}
	if n == 1 {
		return &UnicodeDecodeError{fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", c, pos, reason)}
	}
	return &UnicodeDecodeError{fmt.Sprintf("'utf-8' codec can't decode bytes in position %d-%d: %s", pos, pos+n-1, reason)}
}

// UnicodeDecodeError raised by a strict codec decode, carrying the CPython message
type UnicodeDecodeError struct{ Msg string }

func (e *UnicodeDecodeError) Error() string { return e.Msg }

// Python exception class name the error stands for
func (e *UnicodeDecodeError) PyTypeName() string { return "UnicodeDecodeError" }

// Length of the maximal invalid UTF-8 subpart at the start of b
func UTF8MaximalSubpart(b []byte) int {
	c := b[0]
	lo, hi, need := byte(0x80), byte(0xBF), 0
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		lo, need = 0xA0, 2
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		hi, need = 0x9F, 2
	case c == 0xF0:
		lo, need = 0x90, 3
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	case c == 0xF4:
		hi, need = 0x8F, 3
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(b); k++ {
		if b[n] < lo || b[n] > hi {
			break
		}
		lo, hi = 0x80, 0xBF
		n++
	}
	return n
}
