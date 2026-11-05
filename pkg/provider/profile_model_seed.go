package provider

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Fields endpoint lookups match on, expanded for matching while copied entries keep their env refs
var seedRoutingFields = map[string]bool{
	"name": true, "provider_key": true, "base_url": true, "baseUrl": true, "url": true, "api": true, "enabled": true,
}

// Env expanded model.provider when it is a string, empty and false otherwise
func seedRequestedProvider(modelCfg any) (string, bool) {
	m, ok := modelCfg.(map[string]any)
	if !ok {
		return "", false
	}
	p, ok := m["provider"].(string)
	if !ok {
		return "", false
	}
	return config.Expand(p, os.LookupEnv).(string), true
}

// Entry with its routing fields env expanded, other values and non mappings untouched
func seedExpandRoutingFields(entry any) any {
	m, ok := entry.(map[string]any)
	if !ok {
		return entry
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if seedRoutingFields[k] {
			v = config.Expand(v, os.LookupEnv)
		}
		out[k] = v
	}
	return out
}

// Config with endpoint routing fields expanded, keys and list order kept so lookups address the raw entry
func seedRoutingView(cfg map[string]any) map[string]any {
	view := make(map[string]any, len(cfg))
	for k, v := range cfg {
		view[k] = v
	}
	if providers, ok := cfg["providers"].(map[string]any); ok {
		expanded := make(map[string]any, len(providers))
		for k, v := range providers {
			expanded[k] = seedExpandRoutingFields(v)
		}
		view["providers"] = expanded
	}
	if custom, ok := cfg["custom_providers"].([]any); ok {
		expanded := make([]any, len(custom))
		for i, e := range custom {
			expanded[i] = seedExpandRoutingFields(e)
		}
		view["custom_providers"] = expanded
	}
	return view
}

// Resolver over a routing view that never reads credentials
func seedResolver(ctx context.Context, view map[string]any) *resolver {
	return newResolver(ctx, config.Tree(view), nil, home.Dir(ctx), nil)
}

// Section and key or index of cfg where the resolver found the resolved entry
func seedSourceOf(cfg, resolved map[string]any) (string, any, bool) {
	if key, entry := FindProviderEntry(cfg["providers"], resolved["provider_key"]); entry != nil {
		return "providers", key, true
	}
	custom, _ := cfg["custom_providers"].([]any)
	for index, entry := range custom {
		normalized := NormalizeCustomProviderEntry(entry, "")
		if len(normalized) > 0 && normalized["name"] == resolved["name"] && normalized["base_url"] == resolved["base_url"] {
			return "custom_providers", index, true
		}
	}
	return "", nil, false
}

// Model block plus the endpoint definition it selects, copied as written, empty when the source has no model
func LaunchModelSeed(ctx context.Context, sourceCfg map[string]any) map[string]any {
	modelCfg := sourceCfg["model"]
	if !pyTruthy(modelCfg) {
		return map[string]any{}
	}
	seed := map[string]any{"model": modelCfg}
	prov, ok := seedRequestedProvider(modelCfg)
	if !ok {
		return seed
	}
	requested := normalizeCustomProviderName(prov)
	providers, _ := sourceCfg["providers"].(map[string]any)
	view := seedRoutingView(sourceCfg)
	r := seedResolver(ctx, view)
	selected := r.getNamedCustomProvider(prov, view, false)
	if selected == nil {
		exact, isMap := providers[prov].(map[string]any)
		if isMap && requested != "" && requested != "auto" && (requested == "custom" || r.shadowedByBuiltin(requested)) {
			seed["providers"] = map[string]any{prov: exact}
		}
		return seed
	}
	if section, where, found := seedSourceOf(view, selected); found {
		if section == "providers" {
			seed[section] = map[string]any{where.(string): providers[where.(string)]}
		} else {
			seed[section] = []any{sourceCfg["custom_providers"].([]any)[where.(int)]}
		}
	}
	return seed
}

// Every identity name based lookups accept for an entry
func seedAliases(entry any, key string) map[string]bool {
	if _, ok := entry.(map[string]any); !ok {
		return map[string]bool{}
	}
	m := seedExpandRoutingFields(entry).(map[string]any)
	if key == "" {
		key = pyOrStr(m["provider_key"])
	}
	name := key
	if pyTruthy(m["name"]) {
		name = pyStr(m["name"])
	}
	return CustomProviderAliases(name, key)
}

func aliasesIntersect(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// Copy of the destination config with the seeded endpoint merged over every same identity entry
func MergeEndpointSeed(ctx context.Context, dstCfg, seed map[string]any) map[string]any {
	seedProviders, _ := seed["providers"].(map[string]any)
	if !pyTruthy(seed["providers"]) {
		seedProviders = map[string]any{}
	}
	seedCustom, _ := seed["custom_providers"].([]any)
	claimed := map[string]bool{}
	if prov, ok := seedRequestedProvider(seed["model"]); ok && prov != "" {
		view := seedRoutingView(seed)
		if seedResolver(ctx, view).getNamedCustomProvider(prov, view, false) != nil {
			for k, e := range seedProviders {
				for a := range seedAliases(e, k) {
					claimed[a] = true
				}
			}
			for _, e := range seedCustom {
				for a := range seedAliases(e, "") {
					claimed[a] = true
				}
			}
		}
	}
	merged := make(map[string]any, len(dstCfg))
	for k, v := range dstCfg {
		merged[k] = v
	}
	dstProviders, dstIsMap := dstCfg["providers"].(map[string]any)
	if dstIsMap || len(seedProviders) > 0 {
		out := make(map[string]any, len(seedProviders)+len(dstProviders))
		for k, v := range seedProviders {
			out[k] = v
		}
		for k, v := range dstProviders {
			if _, seeded := seedProviders[k]; !seeded && !aliasesIntersect(claimed, seedAliases(v, k)) {
				out[k] = v
			}
		}
		merged["providers"] = out
	}
	dstCustom, dstIsList := dstCfg["custom_providers"].([]any)
	if dstIsList || len(seedCustom) > 0 {
		out := append([]any{}, seedCustom...)
		for _, e := range dstCustom {
			if !aliasesIntersect(claimed, seedAliases(e, "")) {
				out = append(out, e)
			}
		}
		merged["custom_providers"] = out
	}
	return merged
}
