package pystr

import "unicode/utf8"

// Text of bytes under a strict utf-8 decode that keeps a leading BOM, the UnicodeDecodeError on invalid input
func DecodeUTF8(raw []byte) (string, error) {
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size == 1 {
			return "", UTF8DecodeError(raw, i)
		}
		i += size
	}
	return string(raw), nil
}
