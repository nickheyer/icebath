package charsetutil

import (
	"bytes"
	"encoding/binary"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	replacement   = "\uFFFD"
	byteOrderMark = "\uFEFF"
)

// UTF-8, every invalid run replaced by one U+FFFD in replace mode
type utf8Codec struct{}

func (utf8Codec) decode(data []byte, replace bool) (string, int, string) {
	if utf8.Valid(data) {
		return string(data), -1, ""
	}
	if replace {
		return string(bytes.ToValidUTF8(data, []byte(replacement))), -1, ""
	}
	for i := 0; i < len(data); {
		r, n := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && n == 1 {
			return "", i, utf8Problem(data[i:])
		}
		i += n
	}
	return string(data), -1, ""
}

func (utf8Codec) encode(text string) ([]byte, int) { return []byte(text), -1 }

// What is wrong with the invalid sequence that starts seq: a byte no sequence starts with, a lead byte the end cuts off, or a wrong continuation byte
func utf8Problem(seq []byte) string {
	c := seq[0]
	if c < 0xc2 || c > 0xf4 {
		return "invalid start byte"
	}
	need := 2
	if c >= 0xf0 {
		need = 4
	} else if c >= 0xe0 {
		need = 3
	}
	for k := 1; k < need; k++ {
		if k >= len(seq) {
			return "unexpected end of data"
		}
		lo, hi := byte(0x80), byte(0xbf)
		if k == 1 {
			switch c {
			case 0xe0:
				lo = 0xa0
			case 0xed:
				hi = 0x9f
			case 0xf0:
				lo = 0x90
			case 0xf4:
				hi = 0x8f
			}
		}
		if seq[k] < lo || seq[k] > hi {
			break
		}
	}
	return "invalid continuation byte"
}

// UTF-8 whose leading byte order mark the decoder drops and the encoder writes
type utf8BOMCodec struct{}

func (utf8BOMCodec) decode(data []byte, replace bool) (string, int, string) {
	rest := bytes.TrimPrefix(data, []byte(byteOrderMark))
	text, bad, reason := utf8Codec{}.decode(rest, replace)
	if bad >= 0 {
		bad += len(data) - len(rest)
	}
	return text, bad, reason
}

func (utf8BOMCodec) encode(text string) ([]byte, int) {
	return append([]byte(byteOrderMark), text...), -1
}

// UTF-16 in the byte order big gives, or with bom set in the order a leading byte order mark gives, little endian without one, and a byte order mark written ahead of little endian text
type utf16Codec struct {
	big, bom bool
}

// Byte order of data and the length of the byte order mark that set it
func (c utf16Codec) order(data []byte) (binary.ByteOrder, int) {
	switch {
	case !c.bom && c.big:
		return binary.BigEndian, 0
	case !c.bom:
		return binary.LittleEndian, 0
	case len(data) >= 2 && data[0] == 0xfe && data[1] == 0xff:
		return binary.BigEndian, 2
	case len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe:
		return binary.LittleEndian, 2
	}
	return binary.LittleEndian, 0
}

func (c utf16Codec) decode(data []byte, replace bool) (string, int, string) {
	order, i := c.order(data)
	out := make([]byte, 0, len(data)/2*3)
	for ; i+1 < len(data); i += 2 {
		u := rune(order.Uint16(data[i:]))
		if !utf16.IsSurrogate(u) {
			out = utf8.AppendRune(out, u)
			continue
		}
		if u < 0xdc00 && i+3 < len(data) {
			if low := rune(order.Uint16(data[i+2:])); low >= 0xdc00 && low < 0xe000 {
				out = utf8.AppendRune(out, utf16.DecodeRune(u, low))
				i += 2
				continue
			}
		}
		if !replace {
			return "", i, "unpaired surrogate"
		}
		out = append(out, replacement...)
	}
	if i < len(data) {
		if !replace {
			return "", i, "odd number of bytes"
		}
		out = append(out, replacement...)
	}
	return string(out), -1, ""
}

func (c utf16Codec) encode(text string) ([]byte, int) {
	var order binary.AppendByteOrder = binary.LittleEndian
	if c.big && !c.bom {
		order = binary.BigEndian
	}
	out := make([]byte, 0, 2*len(text)+2)
	if c.bom {
		out = order.AppendUint16(out, 0xfeff)
	}
	for _, r := range text {
		if r >= 0x10000 {
			high, low := utf16.EncodeRune(r)
			out = order.AppendUint16(order.AppendUint16(out, uint16(high)), uint16(low))
			continue
		}
		out = order.AppendUint16(out, uint16(r))
	}
	return out, -1
}

// UTF-32 in the byte order big gives, or with bom set in the order a leading byte order mark gives, little endian without one, and a byte order mark written ahead of little endian text
type utf32Codec struct {
	big, bom bool
}

// Byte order of data and the length of the byte order mark that set it
func (c utf32Codec) order(data []byte) (binary.ByteOrder, int) {
	switch {
	case !c.bom && c.big:
		return binary.BigEndian, 0
	case !c.bom:
		return binary.LittleEndian, 0
	case bytes.HasPrefix(data, []byte{0, 0, 0xfe, 0xff}):
		return binary.BigEndian, 4
	case bytes.HasPrefix(data, []byte{0xff, 0xfe, 0, 0}):
		return binary.LittleEndian, 4
	}
	return binary.LittleEndian, 0
}

func (c utf32Codec) decode(data []byte, replace bool) (string, int, string) {
	order, i := c.order(data)
	out := make([]byte, 0, len(data))
	for ; i+3 < len(data); i += 4 {
		u := order.Uint32(data[i:])
		reason := ""
		switch {
		case u > utf8.MaxRune:
			reason = "code point out of range"
		case utf16.IsSurrogate(rune(u)):
			reason = "surrogate code point"
		default:
			out = utf8.AppendRune(out, rune(u))
			continue
		}
		if !replace {
			return "", i, reason
		}
		out = append(out, replacement...)
	}
	if i < len(data) {
		if !replace {
			return "", i, "truncated code unit"
		}
		out = append(out, replacement...)
	}
	return string(out), -1, ""
}

func (c utf32Codec) encode(text string) ([]byte, int) {
	var order binary.AppendByteOrder = binary.LittleEndian
	if c.big && !c.bom {
		order = binary.BigEndian
	}
	out := make([]byte, 0, 4*len(text)+4)
	if c.bom {
		out = order.AppendUint32(out, 0xfeff)
	}
	for _, r := range text {
		out = order.AppendUint32(out, uint32(r))
	}
	return out, -1
}
