package tools

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Strict UTF-16 text of bytes led by a byte order mark, false when the rest is not valid UTF-16
func decodeUTF16BOM(data []byte) (string, bool) {
	var order binary.ByteOrder
	switch {
	case bytes.HasPrefix(data, []byte{0xff, 0xfe}):
		order = binary.LittleEndian
	case bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		order = binary.BigEndian
	default:
		return "", false
	}
	body := data[2:]
	if len(body)%2 != 0 {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(body); i += 2 {
		u := rune(order.Uint16(body[i:]))
		if !utf16.IsSurrogate(u) {
			b.WriteRune(u)
			continue
		}
		if u >= 0xdc00 || i+3 >= len(body) {
			return "", false
		}
		r := utf16.DecodeRune(u, rune(order.Uint16(body[i+2:])))
		if r == utf8.RuneError {
			return "", false
		}
		b.WriteRune(r)
		i += 2
	}
	return b.String(), true
}
