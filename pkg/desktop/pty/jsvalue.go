package pty

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/desktop/jsjson"
)

// JS String() of a decoded JSON value
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return jsjson.Number(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e != nil {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	}
	return ""
}

// JS truthiness of a decoded JSON value
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x == x && x != 0
	}
	return true
}

// String of value or fallback when value is falsy, the String(value || fallback) idiom
func jsOr(v any, fallback string) string {
	if !jsTruthy(v) {
		return fallback
	}
	return jsString(v)
}

// Value of key with a missing key or a non object record read as nil, the optional chaining read
func get(record any, key string) any {
	m, ok := record.(map[string]any)
	if !ok {
		return nil
	}
	return m[key]
}

func jsSpace(r rune) bool { return unicode.IsSpace(r) || r == 0xFEFF }

// JS String.prototype.trim
func jsTrim(s string) string { return strings.TrimFunc(s, jsSpace) }

var leadingDecimal = regexp.MustCompile(`^[+-]?[0-9]+`)

// Number.parseInt of base ten, false for NaN
func jsParseInt(s string) (int, bool) {
	m := leadingDecimal.FindString(strings.TrimLeftFunc(s, jsSpace))
	if m == "" {
		return 0, false
	}
	n, err := strconv.Atoi(m)
	if err != nil {
		f, ferr := strconv.ParseFloat(m, 64)
		if ferr != nil {
			return 0, false
		}
		if f > 0 {
			return int(^uint(0) >> 1), true
		}
		return -int(^uint(0)>>1) - 1, true
	}
	return n, true
}

// Terminal dimension of a payload value, the Math.max(2, parseInt(String(v || 80)) || 80) clamp
func terminalDimension(v any, fallback int) int {
	n, ok := jsParseInt(jsOr(v, strconv.Itoa(fallback)))
	if !ok || n == 0 {
		n = fallback
	}
	return max(2, n)
}
