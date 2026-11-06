package pystr

import (
	"bytes"
	"unicode/utf8"
)

// Text of utf-8-sig bytes, one leading BOM dropped, the strict UnicodeDecodeError on invalid UTF-8
func DecodeUTF8Sig(raw []byte) (string, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size == 1 {
			return "", UTF8DecodeError(raw, i)
		}
		i += size
	}
	return string(raw), nil
}
