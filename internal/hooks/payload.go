package hooks

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	hooksconfig "github.com/nickheyer/icebath/internal/hooks/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Bounds of a view hooks are handed: how deep it nests, how long its texts and lists run
type caps struct{ depth, text, items int }

// Bounds tried in turn until the view fits the size limit
var tried = []caps{{depth: 8, text: 8000, items: 200}, {depth: 8, text: 1000, items: 50}}

// Least of hooks.payload_max_chars
const minPayload = 1000

// Keys whose values are credentials, never handed to a hook
var sensitive = strs.Set("api_key", "authorization", "proxy_authorization", "cookie", "set_cookie")

// View of a request or response as hooks are handed it: plain JSON data, credentials masked, texts, lists and nesting bounded, tighter bounds
// tried when it runs past hooks.payload_max_chars and a preview of it past that
func Sanitize(cfg *config.Config, value any) any {
	limit := max(cfg.GetInt(hooksconfig.PayloadMaxChars), minPayload)
	data, err := json.Marshal(value)
	if err != nil {
		return strs.Head(fmt.Sprint(value), limit)
	}
	var plain any
	if json.Unmarshal(data, &plain) != nil {
		return strs.Head(string(data), limit)
	}
	encoded := ""
	for _, c := range tried {
		view := bounded(plain, 0, c)
		text, err := json.Marshal(view)
		if err != nil {
			return strs.Head(fmt.Sprint(view), limit)
		}
		if encoded = string(text); utf8.RuneCountInString(encoded) <= limit {
			return view
		}
	}
	return map[string]any{"_truncated": true, "original_type": kindOf(plain), "preview": strs.Head(encoded, limit)}
}

// Value cut to the bounds, credentials masked
func bounded(value any, depth int, c caps) any {
	if depth > c.depth {
		return fmt.Sprintf("<%s depth limit>", kindOf(value))
	}
	switch v := value.(type) {
	case string:
		if dropped := utf8.RuneCountInString(v) - c.text; dropped > 0 {
			return strs.Head(v, c.text) + fmt.Sprintf("...[truncated %d chars]", dropped)
		}
		return v
	case map[string]any:
		out := map[string]any{}
		for i, key := range slices.Sorted(maps.Keys(v)) {
			if i >= c.items {
				out["_truncated_items"] = len(v) - c.items
				break
			}
			if secret(key) {
				out[key] = "<redacted>"
				continue
			}
			out[key] = bounded(v[key], depth+1, c)
		}
		return out
	case []any:
		out := make([]any, 0, min(len(v), c.items)+1)
		for _, item := range v[:min(len(v), c.items)] {
			out = append(out, bounded(item, depth+1, c))
		}
		if len(v) > c.items {
			out = append(out, map[string]any{"_truncated_items": len(v) - c.items})
		}
		return out
	}
	return value
}

// Whether the key names a credential, such as api_key, authorization or any name ending in _api_key
func secret(key string) bool {
	lowered := strings.ReplaceAll(strings.ToLower(key), "-", "_")
	return sensitive[lowered] || strings.HasSuffix(lowered, "_api_key")
}

// Kind of a JSON value as its preview names it
func kindOf(value any) string {
	switch value.(type) {
	case map[string]any:
		return "dict"
	case []any:
		return "list"
	case string:
		return "str"
	case float64:
		return "float"
	case bool:
		return "bool"
	case nil:
		return "NoneType"
	}
	return fmt.Sprintf("%T", value)
}
