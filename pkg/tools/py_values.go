package tools

import (
	"fmt"
	"regexp"
	"strings"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var pyIntLiteralRe = regexp.MustCompile(`^[+-]?\d+$`)

// Ordered JSON object from alternating keys and values, the shape of a Python dict literal
func pyObject(kv ...any) *jsonutil.Object {
	o := jsonutil.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// json.dumps with ensure_ascii false and the default separators
func dumpsUnicode(v any) string {
	out, err := jsonutil.Encode(v)
	if err != nil {
		return ErrorResult(err)
	}
	return out
}

// Strings as a JSON list value
func enumValues(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Integer the way Python int() accepts a JSON value, truncating floats and parsing digit strings
func pyIntValue(v any) (int, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		var out int
		if trimmed := strings.TrimSpace(n); pyIntLiteralRe.MatchString(trimmed) {
			fmt.Sscanf(trimmed, "%d", &out)
			return out, true
		}
	}
	return 0, false
}

// Map view of a value, empty for anything else
func asMapOrEmpty(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Sorted keys of a set
func sortedSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}
