package connections

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Text of a decoded JSON value the way JavaScript String() renders it
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumber(t)
	case int:
		return strconv.Itoa(t)
	case json.Number:
		return t.String()
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			if item != nil {
				parts[i] = jsString(item)
			}
		}
		return strings.Join(parts, ",")
	case []string:
		return strings.Join(t, ",")
	case interface{ String() string }:
		return t.String()
	}
	return "[object Object]"
}

// Number text the way JavaScript renders a double
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
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mantissa, exp, _ := strings.Cut(s, "e")
		if exp[0] == '+' {
			exp = exp[1:]
			return mantissa + "e+" + strings.TrimLeft(exp, "0")
		}
		return mantissa + "e-" + strings.TrimLeft(exp[1:], "0")
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// JavaScript truthiness of a decoded JSON value
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	case int:
		return t != 0
	}
	return true
}

// JavaScript String of the value or an empty string when falsy
func jsOrString(v any) string {
	if !jsTruthy(v) {
		return ""
	}
	return jsString(v)
}

// JavaScript String of the value or an empty string when null
func jsNullishString(v any) string {
	if v == nil {
		return ""
	}
	return jsString(v)
}

// Object view of a decoded JSON value, nil for arrays and scalars
func jsObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// Copy of a JSON object with the given keys set
func jsSpread(base map[string]any, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// JSON text without HTML escaping the way JSON.stringify writes it
func jsonMarshal(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// JSON.stringify equality of two decoded values, key order independent
func jsonEqual(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ab) == string(bb)
}

// Decoded JSON form of a Go value, objects as map[string]any
func toJSONValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}
