package plugins

import "slices"

// Sorted removed Relay plugin identities present in a list, tuple or set config value, none for any other value
func LegacyRelayPluginKeysIn(values any) []string {
	var items []any
	switch v := values.(type) {
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	case map[string]bool:
		for s := range v {
			items = append(items, s)
		}
	case map[string]struct{}:
		for s := range v {
			items = append(items, s)
		}
	case map[any]bool:
		for s := range v {
			items = append(items, s)
		}
	default:
		return []string{}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, item := range items {
		if s, ok := item.(string); ok && legacyRelayPluginKeys[s] && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}
