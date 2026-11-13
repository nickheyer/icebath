package chrome

import (
	"math"
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

// JS Number() of a decoded JSON value
func jsNumber(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case string:
		t := jsTrim(x)
		if t == "" {
			return 0
		}
		if f, err := strconv.ParseFloat(t, 64); err == nil && !strings.ContainsAny(t, "_pP") {
			return f
		}
		return math.NaN()
	}
	return math.NaN()
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
