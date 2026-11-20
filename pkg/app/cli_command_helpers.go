package app

import (
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Ordered JSON object of a plain map, nested maps converted with their keys sorted
func objectFromPlain(m map[string]any) *pyjson.Object {
	out := pyjson.NewObject()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		if nested, ok := v.(map[string]any); ok {
			v = objectFromPlain(nested)
		}
		out.Set(k, v)
	}
	return out
}
