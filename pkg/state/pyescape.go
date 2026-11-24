package state

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
)

// Python str.lower with the full Unicode mapping and final sigma rule
func pyLower(s string) string { return cases.Lower(language.Und).String(s) }

// Python str.upper with the full Unicode mapping that expands characters such as the sharp s
func pyUpper(s string) string { return cases.Upper(language.Und).String(s) }

// Code point count of a string whose lone surrogates are written in generalized UTF-8
func pyLenWTF8(s string) int {
	n := 0
	for i := 0; i < len(s); n++ {
		if _, ok := wtf8Surrogate(s[i:]); ok {
			i += 3
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return n
}

// Bytes of str.encode latin-1 with errors ignore, code points past 0xFF dropped
func latin1Ignore(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if _, ok := wtf8Surrogate(s[i:]); ok {
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r <= 0xFF && !(r == utf8.RuneError && size == 1) {
			out = append(out, byte(r))
		}
		i += size
	}
	return out
}

// String bytes.decode unicode_escape yields, the strict UnicodeDecodeError on a malformed escape
func pyUnicodeEscapeDecode(data []byte) (string, error) {
	text, err := charsetutil.Decode(data, "unicode_escape")
	if err != nil {
		var bad *pycodecs.UnicodeDecodeError
		if errors.As(err, &bad) {
			return "", &pyConvError{"UnicodeDecodeError", fmt.Sprintf("invalid escape at byte %d of a mount path: %s", bad.Pos, bad.Reason)}
		}
		return "", &pyConvError{"UnicodeDecodeError", "invalid escape in a mount path: " + err.Error()}
	}
	return text, nil
}
