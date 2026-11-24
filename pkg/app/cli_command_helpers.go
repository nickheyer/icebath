package app

import (
	"sort"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Ordered JSON object of a plain map, nested maps converted with their keys sorted
func objectFromPlain(m map[string]any) *jsonutil.Object {
	out := jsonutil.NewObject()
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
