package files

import (
	"math"
	"strconv"
	"strings"
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
		return jsNumber(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
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

// JS String() of a number
func jsNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	abs := math.Abs(f)
	if abs >= 1e21 || abs < 1e-6 {
		s := strconv.FormatFloat(f, 'g', -1, 64)
		s = strings.Replace(s, "e+0", "e+", 1)
		s = strings.Replace(s, "e-0", "e-", 1)
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// JS String of a renderer argument with null and undefined read as empty
func jsStringOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return jsString(v)
}

// JS String of a renderer argument with every falsy value read as empty
func jsStringOrFalsy(v any) string {
	if !jsTruthy(v) {
		return ""
	}
	return jsString(v)
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
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	case int64:
		return x != 0
	}
	return true
}
