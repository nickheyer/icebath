package pyjson

import (
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Text of bytes under json.detect_encoding, decoded with surrogatepass, the UnicodeDecodeError on bad input
func DecodeBytes(b []byte) (string, error) {
	enc := "utf-8"
	switch {
	case strings.HasPrefix(string(b), "\x00\x00\xfe\xff") || strings.HasPrefix(string(b), "\xff\xfe\x00\x00"):
		enc = "utf-32"
	case strings.HasPrefix(string(b), "\xfe\xff") || strings.HasPrefix(string(b), "\xff\xfe"):
		enc = "utf-16"
	case strings.HasPrefix(string(b), "\xef\xbb\xbf"):
		return decodeUTF8Strict(b[3:])
	case len(b) >= 4 && b[0] == 0:
		enc = map[bool]string{true: "utf-16-be", false: "utf-32-be"}[b[1] != 0]
	case len(b) >= 4 && b[1] == 0:
		enc = map[bool]string{true: "utf-16-le", false: "utf-32-le"}[b[2] != 0 || b[3] != 0]
	case len(b) == 2 && b[0] == 0:
		enc = "utf-16-be"
	case len(b) == 2 && b[1] == 0:
		enc = "utf-16-le"
	}
	off := 0
	bigEndian := strings.HasSuffix(enc, "-be")
	switch {
	case enc == "utf-8":
		return decodeUTF8Strict(b)
	case strings.HasPrefix(enc, "utf-16"):
		if enc == "utf-16" {
			bigEndian, b, off = b[0] == 0xfe, b[2:], 2
			enc = map[bool]string{true: "utf-16-be", false: "utf-16-le"}[bigEndian]
		}
		if len(b)%2 != 0 {
			return "", codecError(enc, b, off, len(b)-1, len(b), "truncated data")
		}
		var sb strings.Builder
		for i := 0; i < len(b); i += 2 {
			u := rune(b[i+1])<<8 | rune(b[i])
			if bigEndian {
				u = rune(b[i])<<8 | rune(b[i+1])
			}
			if u >= 0xd800 && u <= 0xdbff && i+3 < len(b) {
				lo := rune(b[i+3])<<8 | rune(b[i+2])
				if bigEndian {
					lo = rune(b[i+2])<<8 | rune(b[i+3])
				}
				if lo >= 0xdc00 && lo <= 0xdfff {
					sb.WriteRune(utf16.DecodeRune(u, lo))
					i += 2
					continue
				}
			}
			pystr.WriteRune(&sb, u)
		}
		return sb.String(), nil
	}
	if enc == "utf-32" {
		bigEndian, b, off = b[0] == 0, b[4:], 4
		enc = map[bool]string{true: "utf-32-be", false: "utf-32-le"}[bigEndian]
	}
	if len(b)%4 != 0 {
		return "", codecError(enc, b, off, len(b)-len(b)%4, len(b), "truncated data")
	}
	var sb strings.Builder
	for i := 0; i < len(b); i += 4 {
		u := rune(b[i+3])<<24 | rune(b[i+2])<<16 | rune(b[i+1])<<8 | rune(b[i])
		if bigEndian {
			u = rune(b[i])<<24 | rune(b[i+1])<<16 | rune(b[i+2])<<8 | rune(b[i+3])
		}
		if u < 0 || u > 0x10ffff {
			return "", codecError(enc, b, off, i, i+4, "code point not in range(0x110000)")
		}
		pystr.WriteRune(&sb, u)
	}
	return sb.String(), nil
}

// UTF-8 bytes as text with surrogatepass, the UnicodeDecodeError at the first invalid byte
func decodeUTF8Strict(b []byte) (string, error) {
	if i, ok := pystr.ValidIndex(b); !ok {
		return "", pystr.UTF8DecodeError(b, i)
	}
	return string(b), nil
}

// UnicodeDecodeError over b[start:end] with positions counted from the undecoded input including any BOM
func codecError(enc string, b []byte, off, start, end int, reason string) error {
	if end-start == 1 {
		return &pystr.UnicodeDecodeError{Msg: fmt.Sprintf("'%s' codec can't decode byte 0x%02x in position %d: %s", enc, b[start], off+start, reason)}
	}
	return &pystr.UnicodeDecodeError{Msg: fmt.Sprintf("'%s' codec can't decode bytes in position %d-%d: %s", enc, off+start, off+end-1, reason)}
}
