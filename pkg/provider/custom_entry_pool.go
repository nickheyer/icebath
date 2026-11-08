package provider

import "strings"

// SnapshotOwnerKey is the primary runtime field holding the owning entry's provenance
const SnapshotOwnerKey = "custom_entry"

// RowOwnedByEntry is ownership by provenance, manual rows and rows seeded from the entry's own key env
func RowOwnedByEntry(row PooledCredential, entry map[string]any) bool {
	source := strings.TrimSpace(row.Source)
	for _, k := range []string{"key_env", "api_key_env"} {
		if name := strings.TrimSpace(anyString(entry[k])); name != "" && source == "env:"+name {
			return true
		}
	}
	return isManualSource(source)
}

// EntryScopedPool is the rows the custom entry owns, the pool itself when it owns all, nil when none
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
	view.persistedTokenPairs = pool.persistedTokenPairs
	pool.mu.Unlock()
	return view
}

// PoolForAuxProvider loads a pool for an auxiliary read, entry scoped for a pinned custom name
func PoolForAuxProvider(provider, poolKey string, cfg map[string]any, load func(string) *Pool) *Pool {
	pool := load(poolKey)
	if pool == nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(provider)), CustomPoolPrefix) {
		return pool
	}
	var entry map[string]any
	if match := namedCustomEntry(provider, cfg); match != nil {
		entry = match.entry
	}
	return EntryScopedPool(pool, poolKey, entry)
}

// Whether a provider and requested provider pair names a custom runtime, with its custom name when pinned
func customIdentity(provider, requestedProvider string) (bool, string) {
	custom, name := false, ""
	for _, v := range []string{provider, requestedProvider} {
		id := strings.ToLower(strings.TrimSpace(v))
		if id == "custom" || strings.HasPrefix(id, CustomPoolPrefix) {
			custom = true
		}
		if name == "" && strings.HasPrefix(id, CustomPoolPrefix) {
			name = id
		}
	}
	return custom, name
}

// RuntimeEntryOwner is the credential provenance of the entry owning a custom runtime, names only
func RuntimeEntryOwner(provider, requestedProvider, baseURL string, cfg map[string]any) map[string]any {
	custom, name := customIdentity(provider, requestedProvider)
	if !custom {
		return nil
	}
	match := customEntryForEndpoint(baseURL, name, cfg)
	if match == nil {
		return nil
	}
	owner := map[string]any{"name": match.name, "declares_credential": EntryDeclaresCredential(match.entry)}
	for _, k := range []string{"provider_key", "base_url", "key_env", "api_key_env"} {
		owner[k] = match.entry[k]
	}
	return owner
}

// LoadRuntimePool is the pool for a runtime identity, entry scoped for a custom runtime
func LoadRuntimePool(env PoolEnv, provider, baseURL, requestedProvider string, snapshot map[string]any) *Pool {
	return LoadRuntimePoolWith(env, LoadPool, provider, baseURL, requestedProvider, snapshot)
}

// LoadRuntimePoolWith is LoadRuntimePool reading the resolved pool key through load
func LoadRuntimePoolWith(env PoolEnv, load func(PoolEnv, string) *Pool, provider, baseURL, requestedProvider string, snapshot map[string]any) *Pool {
	cfg := env.config()
	custom, name := customIdentity(provider, requestedProvider)
	if !custom {
		key := ResolveRuntimePoolKey(provider, baseURL, cfg)
		if key == "" {
			return nil
		}
		return load(env, key)
	}
	var owner map[string]any
	if snapshot != nil {
		owner, _ = snapshot[SnapshotOwnerKey].(map[string]any)
	}
	if snapshot != nil && name != "" && owner == nil {
		return nil
	}
	var match *namedEntry
	if owner != nil {
		match = &namedEntry{name: anyString(owner["name"]), entry: owner}
	} else {
		match = customEntryForEndpoint(baseURL, name, cfg)
	}
	key := ""
	if match != nil && (name != "" || owner != nil) {
		if keys := poolKeysForCustomEntry(match.name, match.entry); len(keys) > 0 {
			key = keys[0]
		}
	} else {
		key = ResolveRuntimePoolKey(provider, baseURL, cfg)
	}
	if key == "" {
		return nil
	}
	var entry map[string]any
	if match != nil {
		entry = match.entry
	}
	return EntryScopedPool(load(env, key), key, entry)
}
