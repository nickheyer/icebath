package provider

import "strings"

// Source label of the Anthropic OAuth file icebath writes, and the spelling older builds stored in auth.json
const (
	IcebathPKCESource = "icebath_pkce"
	retiredPKCESource = "hermes_pkce"
)

// Source with the retired OAuth file label renamed, prefixed forms such as manual included
func currentSourceLabel(source string) (string, bool) {
	if source == retiredPKCESource {
		return IcebathPKCESource, true
	}
	if prefix, ok := strings.CutSuffix(source, ":"+retiredPKCESource); ok {
		return prefix + ":" + IcebathPKCESource, true
	}
	return source, false
}

// Renames retired source labels in pool rows and suppression lists in place, reporting any change
func renameRetiredSourceLabels(doc map[string]any) bool {
	changed := false
	pool, _ := doc["credential_pool"].(map[string]any)
	for _, rows := range pool {
		list, _ := rows.([]any)
		for _, row := range list {
			m, ok := row.(map[string]any)
			if !ok {
				continue
			}
			if src, ok := m["source"].(string); ok {
				if renamed, did := currentSourceLabel(src); did {
					m["source"] = renamed
					changed = true
				}
			}
		}
	}
	suppressed, _ := doc["suppressed_sources"].(map[string]any)
	for providerID, listed := range suppressed {
		switch x := listed.(type) {
		case []any:
			kept := make([]any, 0, len(x))
			seen := map[string]bool{}
			renamedAny := false
			for _, v := range x {
				s, ok := v.(string)
				if !ok {
					kept = append(kept, v)
					continue
				}
				renamed, did := currentSourceLabel(s)
				renamedAny = renamedAny || did
				if !seen[renamed] {
					seen[renamed] = true
					kept = append(kept, renamed)
				}
			}
			if renamedAny {
				suppressed[providerID] = kept
				changed = true
			}
		case map[string]any:
			for k, v := range x {
				if renamed, did := currentSourceLabel(k); did {
					delete(x, k)
					if _, has := x[renamed]; !has {
						x[renamed] = v
					}
					changed = true
				}
			}
		}
	}
	return changed
}
