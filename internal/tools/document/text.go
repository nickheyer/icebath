package document

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
)

// Bytes as UTF-8 text, each maximal ill-formed sequence becoming one replacement character as the Unicode standard recommends
func decodeUTF8(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var b strings.Builder
	b.Grow(len(data) + 8)
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			b.Write(data[i : i+size])
			i += size
			continue
		}
		b.WriteRune(utf8.RuneError)
		i += illFormed(data[i:])
	}
	return b.String()
}

// Length of the ill-formed sequence data starts with: its first byte and the following bytes that could still have continued it
func illFormed(data []byte) int {
	need, lo, hi := 0, byte(0x80), byte(0xBF)
	switch c := data[0]; {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		need, hi = 2, 0x9F
	case c == 0xF0:
		need, lo = 3, 0x90
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	case c == 0xF4:
		need, hi = 3, 0x8F
	}
	n := 1
	for n <= need && n < len(data) && data[n] >= lo && data[n] <= hi {
		n++
		lo, hi = 0x80, 0xBF
	}
	return n
}

// Encoding of a text in UTF-16 or UTF-32 told by its first four bytes, a byte order mark or the zero bytes ASCII leaves in its first
// characters, and the length of its mark; nil for UTF-8, with the length of its own mark if it has one
func detect(head []byte) (encoding.Encoding, int) {
	switch {
	case bytes.HasPrefix(head, []byte{0, 0, 0xFE, 0xFF}):
		return utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM), 4
	case bytes.HasPrefix(head, []byte{0xFF, 0xFE, 0, 0}):
		return utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), 4
	case bytes.HasPrefix(head, []byte{0xFE, 0xFF}):
		return unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), 2
	case bytes.HasPrefix(head, []byte{0xFF, 0xFE}):
		return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), 2
	case bytes.HasPrefix(head, []byte{0xEF, 0xBB, 0xBF}):
		return nil, 3
	case len(head) < 4:
		return nil, 0
	case head[0] == 0 && head[1] == 0:
		return utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM), 0
	case head[0] == 0:
		return unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), 0
	case head[1] == 0 && head[2] == 0 && head[3] == 0:
		return utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), 0
	case head[1] == 0:
		return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), 0
	}
	return nil, 0
}

// Text in UTF-8, UTF-16 or UTF-32 as UTF-8 without its byte order mark
func transcode(data []byte) []byte {
	enc, mark := detect(data)
	if enc == nil {
		return data[mark:]
	}
	if out, err := enc.NewDecoder().Bytes(data[mark:]); err == nil {
		return out
	}
	return data[mark:]
}
