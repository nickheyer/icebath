package provider

import "strings"

// Lowercased wire id to the extra tokens appended to the picker search haystack only
var modelSearchAliases = map[string][]string{
	"k3":               {"kimi-k3", "kimi"},
	"x-preview-f-free": {"ox-alpha", "ox"},
}

// Lowercased wire id to its canonical public slug so picker dedup folds a live bare id into its curated row
var modelAliasCanonical = func() map[string]string {
	out := map[string]string{}
	for wireID, aliases := range modelSearchAliases {
		if len(aliases) > 0 {
			out[wireID] = strings.ToLower(aliases[0])
		}
	}
	return out
}()

// ModelAliasCanonical returns the canonical public slug for a bare wire id alias
func ModelAliasCanonical(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	if canonical, ok := modelAliasCanonical[key]; ok {
		return canonical
	}
	return key
}

// ModelSearchText is the haystack for substring model search and never changes the wire id
func ModelSearchText(model string) string {
	mid := strings.TrimSpace(model)
	if mid == "" {
		return model
	}
	aliases, ok := modelSearchAliases[strings.ToLower(mid)]
	if !ok {
		return mid
	}
	return mid + " " + strings.Join(aliases, " ")
}
