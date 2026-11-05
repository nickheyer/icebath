package tools

import (
	"bytes"
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

// Raised when a service definition does not match a recognised icebath launch format
var errServiceIdentityUnverified = errors.New("service_identity_unverified")

// Text of a launcher, UTF-16 when it opens with a BOM and UTF-8 with an optional BOM otherwise
func launcherText(raw []byte) (string, error) {
	if bytes.HasPrefix(raw, []byte{0xff, 0xfe}) || bytes.HasPrefix(raw, []byte{0xfe, 0xff}) {
		text, ok := decodeUTF16BOM(raw)
		if !ok {
			return "", errServiceIdentityUnverified
		}
		return text, nil
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(raw) {
		return "", errServiceIdentityUnverified
	}
	return string(raw), nil
}

// Strict UTF-16 decode led by a byte order mark, false on a truncated unit or an unpaired surrogate
func decodeUTF16BOM(raw []byte) (string, bool) {
	big := raw[0] == 0xfe
	raw = raw[2:]
	if len(raw)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		if big {
			units[i] = uint16(raw[2*i])<<8 | uint16(raw[2*i+1])
		} else {
			units[i] = uint16(raw[2*i+1])<<8 | uint16(raw[2*i])
		}
	}
	for i := 0; i < len(units); i++ {
		switch u := units[i]; {
		case u >= 0xd800 && u < 0xdc00:
			if i+1 >= len(units) || units[i+1] < 0xdc00 || units[i+1] >= 0xe000 {
				return "", false
			}
			i++
		case u >= 0xdc00 && u < 0xe000:
			return "", false
		}
	}
	return string(utf16.Decode(units)), true
}
