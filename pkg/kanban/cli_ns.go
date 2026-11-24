package kanban

import (
	"context"
	"io"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// kanbanCLI is one icebath kanban invocation, its parsed namespace and the writers it prints to
type kanbanCLI struct {
	ctx  context.Context
	ns   map[string]any
	w    io.Writer
	errw io.Writer
}

// Namespace value as a string, empty when absent or not a string
func nsStr(ns map[string]any, key string) string {
	s, _ := ns[key].(string)
	return s
}

// Namespace value as an optional string, nil when absent or None
func nsOptStr(ns map[string]any, key string) *string {
	if s, ok := ns[key].(string); ok {
		return &s
	}
	return nil
}

// Whether a namespace value is set to something other than a zero value
func nsBool(ns map[string]any, key string) bool {
	return anyutil.IsTrue(ns[key])
}

// Namespace value as an optional integer, nil when absent or None
func nsOptInt(ns map[string]any, key string) *int64 {
	v, ok := ns[key]
	if !ok || v == nil {
		return nil
	}
	n, ok := anyutil.Int(v)
	if !ok {
		return nil
	}
	return &n
}

// Namespace value as an optional int, nil when absent or None
func nsOptIntInt(ns map[string]any, key string) *int {
	n := nsOptInt(ns, key)
	if n == nil {
		return nil
	}
	v := int(*n)
	return &v
}

// Namespace value as a float, zero when absent
func nsFloat(ns map[string]any, key string) float64 {
	switch v := ns[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// Namespace value as a list of strings, nil when absent or None
func nsStrs(ns map[string]any, key string) []string {
	switch v := ns[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, anyutil.String(item))
		}
		return out
	case string:
		return []string{v}
	}
	return nil
}
