package provider

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Trimmed base URL with the trailing slash removed, empty for non strings
func normalizedFallbackBaseURL(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// ResolveEntryAPIKey is the key for one fallback entry, inline api_key else key_env through the secret scope
func (r *resolver) ResolveEntryAPIKey(entry map[string]any) string {
	if entry == nil {
		return ""
	}
	if inline := strings.TrimSpace(cfgString(entry["api_key"])); inline != "" {
		return inline
	}
	if keyEnv := strings.TrimSpace(cfgString(entry["key_env"])); keyEnv != "" {
		return strings.TrimSpace(r.secret(keyEnv))
	}
	return ""
}

// Fallback entry key, inline api_key else key_env through the home's secret scope
func ResolveFallbackEntryAPIKey(ctx context.Context, h string, entry map[string]any) string {
	return modelsResolver(ctxForHome(ctx, h)).ResolveEntryAPIKey(entry)
}

// EffectiveRuntimeProvider is the identity to persist for a resolved fallback entry, the configured name over bare custom
func EffectiveRuntimeProvider(entry map[string]any, rt *Runtime) string {
	resolved := ""
	if rt != nil {
		resolved = strings.TrimSpace(rt.Provider)
	}
	if strings.ToLower(resolved) != "custom" {
		return resolved
	}
	requested := ""
	if rt != nil {
		requested = strings.TrimSpace(rt.RequestedProvider)
	}
	if requested == "" && entry != nil {
		requested = strings.TrimSpace(cfgString(entry["provider"]))
	}
	if requested != "" && strings.ToLower(requested) != "custom" {
		return requested
	}
	return resolved
}

// PreAgentFallbackNotice is the one shot line for a provider switch made during credential resolution
func PreAgentFallbackNotice(primaryProvider, primaryModel, fallbackProvider, fallbackModel string) string {
	join := func(parts ...string) string {
		var kept []string
		for _, p := range parts {
			if strings.TrimSpace(p) != "" {
				kept = append(kept, strings.TrimSpace(p))
			}
		}
		return strings.Join(kept, "/")
	}
	primary := join(primaryProvider, primaryModel)
	if primary == "" {
		primary = "primary"
	}
	fallback := join(fallbackProvider, fallbackModel)
	if fallback == "" {
		fallback = "fallback"
	}
	return "⚠️ Provider fallback: " + primary + " unavailable; using " + fallback + " for this response."
}

// Entries with both provider and model, copied with the base URL normalized
func iterFallbackEntries(raw any) []map[string]any {
	var candidates []any
	switch v := raw.(type) {
	case map[string]any:
		candidates = []any{v}
	case []any:
		candidates = v
	case []map[string]any:
		for _, m := range v {
			candidates = append(candidates, m)
		}
	}
	var entries []map[string]any
	for _, c := range candidates {
		entry, ok := c.(map[string]any)
		if !ok {
			continue
		}
		provider := strings.TrimSpace(cfgString(entry["provider"]))
		model := strings.TrimSpace(cfgString(entry["model"]))
		if provider == "" || model == "" {
			continue
		}
		normalized := copyAnyMap(entry)
		normalized["provider"] = provider
		normalized["model"] = model
		if baseURL := normalizedFallbackBaseURL(entry["base_url"]); baseURL != "" {
			normalized["base_url"] = baseURL
		}
		entries = append(entries, normalized)
	}
	return entries
}

// Identity of a fallback entry, canonical provider id, lower model, and lower base URL
func fallbackEntryIdentity(entry map[string]any) [3]string {
	return [3]string{
		NormalizeProvider(strings.TrimSpace(cfgString(entry["provider"]))),
		strings.ToLower(strings.TrimSpace(cfgString(entry["model"]))),
		strings.ToLower(normalizedFallbackBaseURL(entry["base_url"])),
	}
}

// FallbackChainOf is the chain a fallback_providers value names, same route duplicates dropped
func FallbackChainOf(value any) []map[string]any {
	var chain []map[string]any
	seen := map[[3]string]bool{}
	for _, entry := range iterFallbackEntries(value) {
		identity := fallbackEntryIdentity(entry)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		chain = append(chain, entry)
	}
	return chain
}

// Fallback chain in effect for ctx, same route duplicates dropped
func FallbackChain(ctx context.Context) []map[string]any {
	return FallbackChainOf(settings.FallbackModels.Get(ctx))
}

// ScopedFallbackChain picks the chain for a route owner that can pin its own primary, nil disabling fallback
func ScopedFallbackChain(inherited []map[string]any, declared any, declaredSet bool, pinned bool, owner string) []map[string]any {
	var def []map[string]any
	if !pinned && len(inherited) > 0 {
		def = inherited
	}
	if !declaredSet || declared == nil {
		return def
	}
	if list, ok := asList(declared); ok && len(list) == 0 {
		return nil
	}
	normalized := FallbackChainOf(declared)
	if len(normalized) == 0 {
		which := "inherited"
		if pinned {
			which = "pinned"
		}
		pkgLog().Warn(owner + " fallback_providers has no usable routes; using the " + which + " default")
		return def
	}
	return normalized
}
