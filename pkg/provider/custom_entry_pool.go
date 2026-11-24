package provider

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
)

// SnapshotOwnerKey is the primary runtime field holding the owning entry's provenance
const SnapshotOwnerKey = "custom_entry"

// RowOwnedByEntry is ownership by provenance, manual rows and rows seeded from the entry's own key env
func RowOwnedByEntry(row PooledCredential, entry map[string]any) bool {
	source := strings.TrimSpace(row.Source)
	for _, k := range []string{"key_env"} {
		if name := strings.TrimSpace(anyString(entry[k])); name != "" && source == "env:"+name {
			return true
		}
	}
	return isManualSource(source)
}

// EntryScopedPool is the rows the custom entry owns with the login choice the pool was narrowed to, the pool itself when it owns all, nil when none
func EntryScopedPool(pool *Pool, poolKey string, entry map[string]any) *Pool {
	if pool == nil || strings.HasPrefix(poolKey, CustomPoolPrefix) || !EntryDeclaresCredential(entry) {
		return pool
	}
	rows := pool.Entries()
	var owned []PooledCredential
	for _, row := range rows {
		if RowOwnedByEntry(row, entry) {
			owned = append(owned, row)
		}
	}
	if len(owned) == len(rows) {
		return pool
	}
	if len(owned) == 0 {
		return nil
	}
	view := NewPool(pool.env, pool.provider, owned)
	pool.mu.Lock()
	for id, pair := range pool.persistedTokenPairs {
		view.persistedTokenPairs[id] = pair
	}
	if pool.choice != nil {
		choice := *pool.choice
		view.choice = &choice
	}
	for _, e := range pool.spare {
		if RowOwnedByEntry(e, entry) {
			view.spare = append(view.spare, e)
		}
	}
	pool.mu.Unlock()
	return view
}

// PoolForAuxProvider loads a pool for an auxiliary read, entry scoped when the provider or its pool key names a custom entry of the providers setting ctx gives
func PoolForAuxProvider(ctx context.Context, provider, poolKey string, load func(string) *Pool) *Pool {
	pool := load(poolKey)
	if pool == nil {
		return pool
	}
	match := customEntryForIdentity(ctx, provider)
	if match == nil {
		match = customEntryForIdentity(ctx, poolKey)
	}
	if match == nil {
		return pool
	}
	return EntryScopedPool(pool, poolKey, match.entry)
}

// Whether config can resolve an identity to a custom entry, false for empty, auto, bare custom and main
func mayNameCustomEntry(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "auto", "custom", "main":
		return false
	}
	return true
}

// Built-in provider's own address when poolKey is a built-in's pool id, false when the pool belongs to a custom entry alone
func builtinPoolAddress(ctx context.Context, poolKey string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(poolKey))
	if key == "" || strings.HasPrefix(key, CustomPoolPrefix) {
		return "", false
	}
	if key == "openrouter" {
		return OpenRouterBaseURL, true
	}
	row := providerRegistry().get(ctx, key)
	if row == nil {
		return "", false
	}
	return registryInferenceBaseURL(row), true
}

// Custom endpoint of the providers setting ctx gives that a provider identity names the way the runtime resolves it, nil for a built-in, bare custom and auto
func customEntryForIdentity(ctx context.Context, provider string) *namedEntry {
	name := strings.ToLower(strings.TrimSpace(provider))
	if !mayNameCustomEntry(name) {
		return nil
	}
	providers := settings.Providers.Get(ctx)
	if strings.HasPrefix(name, CustomPoolPrefix) {
		return namedCustomEntry(name, providers)
	}
	path := modelsResolver(ctx).NamedCustomProviderConfigPath(name, providers)
	if path == nil {
		return nil
	}
	if path.Section != "providers" {
		return namedCustomEntry(name, providers)
	}
	key := normalizeCustomPoolName(path.Key)
	for _, ne := range iterCustomProviders(providers) {
		if normalizeCustomPoolName(cfgString(ne.entry["provider_key"])) == key {
			found := ne
			return &found
		}
	}
	return nil
}

// Whether a provider and requested provider pair names a custom runtime at baseURL under the providers setting ctx gives, with the entry's identity when one names it
func customIdentity(ctx context.Context, provider, requestedProvider, baseURL string) (bool, string) {
	identities := []string{strings.ToLower(strings.TrimSpace(provider)), strings.ToLower(strings.TrimSpace(requestedProvider))}
	namesEntry := func(v string) bool {
		if customEntryForIdentity(ctx, v) == nil {
			return false
		}
		if _, builtin := builtinPoolAddress(ctx, v); !builtin {
			return true
		}
		return customEntryForEndpoint(baseURL, v, settings.Providers.Get(ctx)) != nil
	}
	name := ""
	for _, v := range identities {
		if strings.HasPrefix(v, CustomPoolPrefix) {
			name = v
			break
		}
	}
	if name == "" {
		for _, v := range identities {
			if namesEntry(v) {
				name = v
				break
			}
		}
	}
	return name != "" || identities[0] == "custom" || identities[1] == "custom", name
}

// IsCustomRuntime reports a runtime identity that is a custom entry's, whose pool comes from LoadRuntimePool and never a raw LoadPool of a slug a built-in may share
func IsCustomRuntime(env PoolEnv, provider, requestedProvider, baseURL string) bool {
	custom, _ := customIdentity(env.hookContext(), provider, requestedProvider, baseURL)
	return custom
}

// RuntimeEntryOwner is the credential provenance of the entry of the providers setting ctx gives that owns a custom runtime, names only
func RuntimeEntryOwner(ctx context.Context, provider, requestedProvider, baseURL string) map[string]any {
	custom, name := customIdentity(ctx, provider, requestedProvider, baseURL)
	if !custom {
		return nil
	}
	match := customEntryForEndpoint(baseURL, name, settings.Providers.Get(ctx))
	if match == nil {
		return nil
	}
	owner := map[string]any{"name": match.name, "declares_credential": EntryDeclaresCredential(match.entry)}
	for _, k := range []string{"provider_key", "base_url", "key_env"} {
		owner[k] = match.entry[k]
	}
	return owner
}

// LoadRuntimePool is the pool for a runtime identity, entry scoped for a custom runtime and narrowed to the selected login
func LoadRuntimePool(env PoolEnv, provider, baseURL, requestedProvider string, snapshot map[string]any) *Pool {
	return LoadRuntimePoolWith(env, LoadSelectedPool, provider, baseURL, requestedProvider, snapshot)
}

// Pool key of a runtime identity and the custom entry owning it, empty when no pool fits
func runtimePoolKey(env PoolEnv, provider, baseURL, requestedProvider string, snapshot map[string]any) (string, map[string]any, bool) {
	ctx := env.hookContext()
	providers := settings.Providers.Get(ctx)
	custom, name := customIdentity(ctx, provider, requestedProvider, baseURL)
	if !custom {
		return ResolveRuntimePoolKey(provider, baseURL, providers), nil, false
	}
	var owner map[string]any
	if snapshot != nil {
		owner, _ = snapshot[SnapshotOwnerKey].(map[string]any)
	}
	if snapshot != nil && name != "" && owner == nil {
		return "", nil, true
	}
	var match *namedEntry
	if owner != nil {
		match = &namedEntry{name: anyString(owner["name"]), entry: owner}
	} else {
		match = customEntryForEndpoint(baseURL, name, providers)
	}
	if name != "" && match == nil {
		return "", nil, true
	}
	key := ""
	if match != nil && (name != "" || owner != nil) {
		if keys := poolKeysForCustomEntry(match.name, match.entry); len(keys) > 0 {
			key = keys[0]
		}
	} else {
		key = ResolveRuntimePoolKey(provider, baseURL, providers)
	}
	var entry map[string]any
	if match != nil {
		entry = match.entry
	}
	return key, entry, true
}

// RoutePoolKey is the pool key of the logins a route's requests use, a named custom entry's own key when the route names no endpoint, empty when none fits
func RoutePoolKey(env PoolEnv, provider, baseURL, requestedProvider string) string {
	key, _, _ := runtimePoolKey(env, provider, baseURL, requestedProvider, nil)
	if key != "" || baseURL != "" {
		return key
	}
	for _, name := range []string{provider, requestedProvider} {
		if named := namedCustomEntry(name, settings.Providers.Get(env.hookContext())); named != nil {
			if keys := poolKeysForCustomEntry(named.name, named.entry); len(keys) > 0 {
				return keys[0]
			}
		}
	}
	return ""
}

// LoadRuntimePoolWith is LoadRuntimePool reading the resolved pool key through load
func LoadRuntimePoolWith(env PoolEnv, load func(PoolEnv, string) *Pool, provider, baseURL, requestedProvider string, snapshot map[string]any) *Pool {
	key, entry, custom := runtimePoolKey(env, provider, baseURL, requestedProvider, snapshot)
	if key == "" {
		return nil
	}
	if !custom {
		return load(env, key)
	}
	return EntryScopedPool(load(env, key), key, entry)
}

// LoadRoutePool is the selected login's pool to attach when a live agent moves onto a route, entry scoped for a custom identity
func LoadRoutePool(env PoolEnv, provider, baseURL, requestedProvider string) *Pool {
	if IsCustomRuntime(env, provider, requestedProvider, baseURL) {
		return LoadRuntimePool(env, provider, baseURL, requestedProvider, nil)
	}
	return LoadSelectedPool(env, strings.ToLower(strings.TrimSpace(provider)))
}

// LoadRoutePoolWith is LoadRoutePool reading every pool through load
func LoadRoutePoolWith(env PoolEnv, load func(PoolEnv, string) *Pool, provider, baseURL, requestedProvider string) *Pool {
	if IsCustomRuntime(env, provider, requestedProvider, baseURL) {
		return LoadRuntimePoolWith(env, load, provider, baseURL, requestedProvider, nil)
	}
	return load(env, strings.ToLower(strings.TrimSpace(provider)))
}
