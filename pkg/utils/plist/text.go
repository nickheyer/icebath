package plistutil

import (
	"bytes"
	"encoding/binary"
	"unicode/utf16"
	"unicode/utf8"
)

// UTF-8 bytes of a document marked by a byte order mark, true when they were transcoded from UTF-16 or UTF-32
func utf8Document(data []byte) ([]byte, bool, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		return data[3:], false, nil
	case bytes.HasPrefix(data, []byte{0x00, 0x00, 0xFE, 0xFF}):
		text, err := utf32Text(data[4:], binary.BigEndian)
		return text, true, err
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE, 0x00, 0x00}):
		text, err := utf32Text(data[4:], binary.LittleEndian)
		return text, true, err
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		text, err := utf16Text(data[2:], binary.BigEndian)
		return text, true, err
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		text, err := utf16Text(data[2:], binary.LittleEndian)
		return text, true, err
	}
	return data, false, nil
}

func utf16Text(body []byte, order binary.ByteOrder) ([]byte, error) {
	if len(body)%2 != 0 {
		return nil, &Error{Msg: "truncated UTF-16 text"}
	}
	return appendUTF16(make([]byte, 0, len(body)), body, order), nil
}

func utf32Text(body []byte, order binary.ByteOrder) ([]byte, error) {
	if len(body)%4 != 0 {
		return nil, &Error{Msg: "truncated UTF-32 text"}
	}
	out := make([]byte, 0, len(body))
	for i := 0; i < len(body); i += 4 {
		out = utf8.AppendRune(out, rune(order.Uint32(body[i:])))
	}
	return out, nil
}

// UTF-8 of UTF-16 code units appended to out, a lone surrogate becoming U+FFFD
func appendUTF16(out, body []byte, order binary.ByteOrder) []byte {
	for i := 0; i+1 < len(body); i += 2 {
		r := rune(order.Uint16(body[i:]))
		if utf16.IsSurrogate(r) && i+3 < len(body) {
			if pair := utf16.DecodeRune(r, rune(order.Uint16(body[i+2:]))); pair != utf8.RuneError {
				r = pair
				i += 2
			}
		}
		out = utf8.AppendRune(out, r)
	}
	return out
}
