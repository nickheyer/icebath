package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Fields endpoint lookups match on, expanded for matching while copied entries keep their env refs
var seedRoutingFields = map[string]bool{"name": true, "provider_key": true, "base_url": true, "enabled": true}

// Identity and eligibility fields an inherited seed fixes in the source scope, URL and credential refs staying raw
var seedIdentityFields = map[string]bool{"name": true, "provider_key": true, "enabled": true}

// Env expanded model.provider when it is a string, empty and false otherwise
func seedRequestedProvider(ctx context.Context, modelCfg any) (string, bool) {
	m, ok := modelCfg.(map[string]any)
	if !ok {
		return "", false
	}
	p, ok := m["provider"].(string)
	if !ok {
		return "", false
	}
	return config.ExpandString(p, config.EnvRefLookupContext(ctx)), true
}

// Entry with the named fields env expanded, other values, key order and non mappings untouched
func seedExpandFields(ctx context.Context, entry any, fields map[string]bool) any {
	m, ok := entry.(map[string]any)
	if !ok {
		return entry
	}
	lookup := config.EnvRefLookupContext(ctx)
	out := make(map[string]any, len(m))
	for k, v := range m {
		if fields[k] {
			v = config.Expand(v, lookup)
		}
		out[k] = v
	}
	config.SetMappingKeys(out, config.MappingKeys(m))
	return out
}

// Config with endpoint routing fields expanded, keys and list order kept so lookups address the raw entry
func seedRoutingView(ctx context.Context, cfg map[string]any) map[string]any {
	view := make(map[string]any, len(cfg))
	for k, v := range cfg {
		view[k] = v
	}
	config.SetMappingKeys(view, config.MappingKeys(cfg))
	if providers, ok := cfg["providers"].(map[string]any); ok {
		expanded := make(map[string]any, len(providers))
		for k, v := range providers {
			expanded[k] = seedExpandFields(ctx, v, seedRoutingFields)
		}
		config.SetMappingKeys(expanded, config.MappingKeys(providers))
		view["providers"] = expanded
	}
	return view
}

// Resolver for routing a source document's entries that never reads credentials
func seedResolver(ctx context.Context) *resolver {
	return newResolver(ctx, nil, home.Dir(ctx), nil)
}

// Model block plus the endpoint definition it selects, copied as written, empty when the source has no model
func LaunchModelSeed(ctx context.Context, sourceCfg map[string]any) map[string]any {
	seed, _ := selectedModelSeed(ctx, sourceCfg)
	return seed
}

// Model seed of a source config and whether its provider selects an endpoint the runtime resolves
func selectedModelSeed(ctx context.Context, sourceCfg map[string]any) (map[string]any, bool) {
	modelCfg := sourceCfg["model"]
	if !pyTruthy(modelCfg) {
		return map[string]any{}, false
	}
	seed := map[string]any{"model": modelCfg}
	prov, ok := seedRequestedProvider(ctx, modelCfg)
	if !ok {
		return seed, false
	}
	requested := normalizeCustomProviderName(prov)
	providers, _ := sourceCfg["providers"].(map[string]any)
	view := seedRoutingView(ctx, sourceCfg)
	r := seedResolver(ctx)
	found := r.NamedCustomProviderConfigPath(prov, config.AsMap(view["providers"]))
	if found == nil {
		exact, isMap := providers[prov].(map[string]any)
		if isMap && requested != "" && requested != "auto" && (requested == "custom" || r.shadowedByBuiltin(requested)) {
			assignKey(seed, "providers", map[string]any{prov: exact})
		}
		return seed, false
	}
	assignKey(seed, "providers", map[string]any{found.Key: providers[found.Key]})
	return seed, true
}

// Every identity name based lookups accept for an entry
func seedAliases(ctx context.Context, entry any, key string) map[string]bool {
	if _, ok := entry.(map[string]any); !ok {
		return map[string]bool{}
	}
	m := seedExpandFields(ctx, entry, seedRoutingFields).(map[string]any)
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

// Every identity the endpoint entries of a seed claim
func seedClaimedAliases(ctx context.Context, seed map[string]any) map[string]bool {
	claimed := map[string]bool{}
	if providers, ok := seed["providers"].(map[string]any); ok {
		for k, e := range providers {
			for a := range seedAliases(ctx, e, k) {
				claimed[a] = true
			}
		}
	}
	return claimed
}

// Seed and the identities it claims, both fixed in the source scope so a merge never resolves them in the destination's
func PrepareInheritedEndpointSeed(ctx context.Context, sourceCfg map[string]any) (map[string]any, map[string]bool) {
	seed, selected := selectedModelSeed(ctx, sourceCfg)
	if !selected {
		return seed, map[string]bool{}
	}
	if providers, ok := seed["providers"].(map[string]any); ok {
		frozen := make(map[string]any, len(providers))
		for k, v := range providers {
			frozen[k] = seedExpandFields(ctx, v, seedIdentityFields)
		}
		config.SetMappingKeys(frozen, config.MappingKeys(providers))
		seed["providers"] = frozen
	}
	return seed, seedClaimedAliases(ctx, seed)
}

// Copy of the destination config with the seeded endpoint first and every entry claiming its identities dropped, nil claims resolved here
func MergeEndpointSeed(ctx context.Context, dstCfg, seed map[string]any, claimed map[string]bool) map[string]any {
	if claimed == nil {
		claimed = map[string]bool{}
		if prov, ok := seedRequestedProvider(ctx, seed["model"]); ok && prov != "" {
			view := seedRoutingView(ctx, seed)
			if seedResolver(ctx).getNamedCustomProvider(prov, config.AsMap(view["providers"]), false) != nil {
				claimed = seedClaimedAliases(ctx, seed)
			}
		}
	}
	return mergeEndpointSeed(ctx, dstCfg, seed, claimed)
}

// Destination config with the seed's providers entries written first, same key and claimed identity entries removed
func mergeEndpointSeed(ctx context.Context, dstCfg, seed map[string]any, claimed map[string]bool) map[string]any {
	seedProviders, _ := seed["providers"].(map[string]any)
	merged := make(map[string]any, len(dstCfg))
	for k, v := range dstCfg {
		merged[k] = v
	}
	config.SetMappingKeys(merged, config.MappingKeys(dstCfg))
	dstProviders, dstIsMap := dstCfg["providers"].(map[string]any)
	if dstIsMap || len(seedProviders) > 0 {
		out := make(map[string]any, len(seedProviders)+len(dstProviders))
		order := config.MappingKeys(seedProviders)
		for k, v := range seedProviders {
			out[k] = v
		}
		for _, k := range config.MappingKeys(dstProviders) {
			if _, seeded := seedProviders[k]; !seeded && !aliasesIntersect(claimed, seedAliases(ctx, dstProviders[k], k)) {
				out[k] = dstProviders[k]
				order = append(order, k)
			}
		}
		config.SetMappingKeys(out, order)
		assignKey(merged, "providers", out)
	}
	return merged
}
