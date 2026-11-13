package system

import (
	"strings"

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

// JS String of value or fallback when value is falsy, the value || fallback idiom
func jsOr(v any, fallback string) string {
	if !jsTruthy(v) {
		return fallback
	}
	return jsString(v)
}

// Value of key, nil and false when the record lacks it or is no object, the optional chaining read
func field(record any, key string) (any, bool) {
	m, ok := record.(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}

// Value of key with a missing key read as nil
func get(record any, key string) any {
	v, _ := field(record, key)
	return v
}

// First of the values that is not null or undefined, the a ?? b chain
func coalesce(record any, keys ...string) any {
	for _, key := range keys {
		if v := get(record, key); v != nil {
			return v
		}
	}
	return nil
}
