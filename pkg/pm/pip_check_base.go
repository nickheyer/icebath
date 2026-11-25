package pm

import (
	"strings"
	"unicode/utf8"
)

// Python truthiness of a decoded value
func tomlTruthy(v any) bool {
	switch t := v.(type) {
	case string:
		return t != ""
	case bool:
		return t
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case *OrderedObject:
		return len(t.Keys) > 0
	}
	return true
}

// Joined report lines kept to their last characters the way a Python slice keeps them
func lastChars(s string, n int) string {
	start, count := len(s), 0
	for start > 0 && count < n {
		_, width := utf8.DecodeLastRuneInString(s[:start])
		start -= width
		count++
	}
	if start == 0 {
		return s
	}
	if tail := s[start:]; utf8.ValidString(tail) {
		return strings.Clone(tail)
	}
	return string([]rune(s[start:]))
}

// Refuses bytes that are not UTF-8 with the UnicodeDecodeError Python raises
func utf8Strict(data []byte) error {
	if err := strictUTF8(data); err != nil {
		return &ClassError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	return nil
}
