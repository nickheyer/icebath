package provider

import (
	"context"
	"strings"
	"sync"
)

// Providers kept out of the plugin mirror, bespoke refresh or aggregator and user supplied routes
var registryPluginSkip = map[string]bool{"kimi-coding": true, "kimi-coding-cn": true, "zai": true, "openrouter": true, "custom": true}

// Names whose registry row came from a plugin profile rather than the built in rows
var (
	pluginMirroredMu        sync.RWMutex
	pluginMirroredProviders = map[string]bool{}
)

// The provider registry, process wide storage whose rows each read as the bound home sees them
type homeScopedRegistry struct {
	mu          sync.RWMutex
	keys        []string
	rows        map[string]*Profile
	builtin     map[string]bool
	syncPending bool
	syncing     sync.Map
}

var (
	authRegistryOnce sync.Once
	authRegistry     *homeScopedRegistry
	homeRowsMu       sync.Mutex
	homeRows         = map[[2]*Profile]*Profile{}
)

// The built in rows in priority order with the mirror armed for the first read
func providerRegistry() *homeScopedRegistry {
	authRegistryOnce.Do(func() {
		reg := &homeScopedRegistry{rows: map[string]*Profile{}, builtin: map[string]bool{}}
		rows := append(registryRows(), moaVirtualRow())
		for _, r := range rows {
			p := r.profile()
			normalizeRegisteredProfile(&p)
			reg.keys = append(reg.keys, p.Name)
			reg.rows[p.Name] = &p
			reg.builtin[p.Name] = true
		}
		reg.syncPending = true
		authRegistry = reg
		authRegistryReady.Store(true)
	})
	return authRegistry
}

func pluginMirroredProvider(provider string) bool {
	pluginMirroredMu.RLock()
	defer pluginMirroredMu.RUnlock()
	return pluginMirroredProviders[provider]
}

// The registry auth type of a profile, registry_auth_type winning over auth_type
func profileRegistryAuthType(p Profile) string {
	if p.RegistryAuthType != "" {
		return p.RegistryAuthType
	}
	return p.AuthType
}

// Row stored under key as the bound home sees it, nil when hidden there
func (r *homeScopedRegistry) view(key string, row *Profile, resolve profileResolve) *Profile {
	if key != row.Name {
		if claimant, _ := resolve(key); claimant != nil && claimant.Name != row.Name {
			row = r.rawGet(claimant.Name)
			if row == nil || row.Name != claimant.Name {
				return nil
			}
		}
	}
	user, shared := resolve(row.Name)
	mirrored := pluginMirroredProvider(row.Name)
	if user == nil {
		if shared != nil || !mirrored {
			return row
		}
		return nil
	}
	if registryPluginSkip[row.Name] || !(mirrored || r.builtin[row.Name]) {
		return row
	}
	return homeRow(row, user, mirrored && shared == nil)
}

// Runs the pending mirror before a read, never while discovery is mid scan or inside the mirror itself
func (r *homeScopedRegistry) ready(ctx context.Context) {
	r.mu.RLock()
	pending := r.syncPending
	r.mu.RUnlock()
	gid := goroutineID()
	if _, active := r.syncing.Load(gid); !pending || active || discoveringNow() {
		return
	}
	r.syncing.Store(gid, true)
	syncPluginProviderRegistry(ctx)
	r.syncing.Delete(gid)
	r.mu.Lock()
	r.syncPending = false
	r.mu.Unlock()
}

func (r *homeScopedRegistry) rawGet(key string) *Profile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rows[key]
}

func (r *homeScopedRegistry) rawContains(key string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.rows[key]
	return ok
}

// Stores a row under key, a new key appended and an existing one keeping its place
func (r *homeScopedRegistry) set(key string, row *Profile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rows[key]; !ok {
		r.keys = append(r.keys, key)
	}
	r.rows[key] = row
}

// Bound home view of one key with the pending mirror run first
func (r *homeScopedRegistry) get(ctx context.Context, key string) *Profile {
	r.ready(ctx)
	row := r.rawGet(key)
	if row == nil {
		return nil
	}
	return r.view(key, row, boundProfileResolver(ctx))
}

func (r *homeScopedRegistry) contains(ctx context.Context, key string) bool {
	return r.get(ctx, key) != nil
}

type registryItem struct {
	Key string
	Row *Profile
}

// Every key with its bound home view in storage order, hidden rows left out
func (r *homeScopedRegistry) items(ctx context.Context) []registryItem {
	r.ready(ctx)
	resolve := boundProfileResolver(ctx)
	r.mu.RLock()
	stored := make([]registryItem, 0, len(r.keys))
	for _, k := range r.keys {
		stored = append(stored, registryItem{k, r.rows[k]})
	}
	r.mu.RUnlock()
	out := make([]registryItem, 0, len(stored))
	for _, it := range stored {
		if v := r.view(it.Key, it.Row, resolve); v != nil {
			out = append(out, registryItem{it.Key, v})
		}
	}
	return out
}

// Static inference base URL of a registry row, empty for Vertex whose endpoint is computed per request
func registryInferenceBaseURL(p *Profile) string {
	if p.AuthType == AuthTypeVertex {
		return ""
	}
	return p.BaseURL
}

// Inference base URL of a provider registry id and whether the id is registered
func RegistryInferenceBaseURL(id string) (string, bool) {
	row := providerRegistry().get(context.Background(), id)
	if row == nil {
		return "", false
	}
	return registryInferenceBaseURL(row), true
}

// Every provider registry entry's inference base URL in registry order
func RegistryInferenceBaseURLs() []string {
	var out []string
	for _, it := range providerRegistry().items(context.Background()) {
		out = append(out, registryInferenceBaseURL(it.Row))
	}
	return out
}

// Row a home whose own plugin declares the row's provider reads, cached per stored row and profile
func homeRow(row, pp *Profile, fromProfile bool) *Profile {
	key := [2]*Profile{row, pp}
	homeRowsMu.Lock()
	cached, ok := homeRows[key]
	homeRowsMu.Unlock()
	if ok {
		return cached
	}
	var out *Profile
	if fromProfile {
		out = profileRegistryRow(*pp)
	}
	if out == nil {
		cp := *row
		out = &cp
		overrideRegistryRow(out, *pp)
	}
	homeRowsMu.Lock()
	if len(homeRows) >= 512 {
		homeRows = map[[2]*Profile]*Profile{}
	}
	homeRows[key] = out
	homeRowsMu.Unlock()
	return out
}

// Splits env vars into api key vars and the first base url var, all vars when none is a key var
func apiKeyEnvFields(pp Profile) ([]string, string) {
	isURL := func(v string) bool { return strings.HasSuffix(v, "_BASE_URL") || strings.HasSuffix(v, "_URL") }
	var keys []string
	urlVar := ""
	for _, v := range pp.KeyEnv {
		if isURL(v) {
			if urlVar == "" {
				urlVar = v
			}
			continue
		}
		keys = append(keys, v)
	}
	if len(keys) == 0 {
		keys = append([]string(nil), pp.KeyEnv...)
	}
	return keys, urlVar
}

// Mirrors one profile into the registry under the auth type it declares
func registerPluginProvider(ctx context.Context, pp *Profile) {
	row := profileRegistryRow(*pp)
	if row == nil {
		return
	}
	insertRegistryRow(row, pp.RegistryAfter)
	pluginMirroredMu.Lock()
	pluginMirroredProviders[pp.Name] = true
	pluginMirroredMu.Unlock()
	mirrorAliases(ctx, row, pp)
}

// Registry row a profile declares, nil for a skipped name or an api key profile without env vars
func profileRegistryRow(pp Profile) *Profile {
	if registryPluginSkip[pp.Name] {
		return nil
	}
	row := pp
	authType := profileRegistryAuthType(pp)
	if row.DisplayName == "" {
		row.DisplayName = pp.Name
	}
	row.AuthType = authType
	row.Auth = AuthKindOf(authType)
	if authType == AuthTypeAPIKey {
		if len(pp.KeyEnv) == 0 {
			return nil
		}
		keyVars, urlVar := apiKeyEnvFields(pp)
		row.KeyEnv = keyVars
		if row.BaseURLEnv == "" {
			row.BaseURLEnv = urlVar
		}
		return &row
	}
	row.KeyEnv = nil
	return &row
}

// Adds a row right after the named row, appended when that row is absent
func insertRegistryRow(row *Profile, after string) {
	reg := providerRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if _, ok := reg.rows[after]; !ok || after == "" {
		if _, exists := reg.rows[row.Name]; !exists {
			reg.keys = append(reg.keys, row.Name)
		}
		reg.rows[row.Name] = row
		return
	}
	at := 0
	for i, k := range reg.keys {
		if k == after {
			at = i + 1
			break
		}
	}
	type kv struct {
		k string
		v *Profile
	}
	var ordered []kv
	for _, k := range reg.keys[:at] {
		ordered = append(ordered, kv{k, reg.rows[k]})
	}
	ordered = append(ordered, kv{row.Name, row})
	for _, k := range reg.keys[at:] {
		ordered = append(ordered, kv{k, reg.rows[k]})
	}
	reg.keys = nil
	reg.rows = map[string]*Profile{}
	for _, e := range ordered {
		if _, seen := reg.rows[e.k]; !seen {
			reg.keys = append(reg.keys, e.k)
		}
		reg.rows[e.k] = e.v
	}
}

// True when the alias resolves to the profile and that profile is a home plugin
func userOwnsAlias(ctx context.Context, pp *Profile, alias string) bool {
	owner := getProviderProfile(ctx, alias)
	return owner != nil && owner.Name == pp.Name && ProviderSource(ctx, pp.Name) == "user"
}

// Points the profile's aliases at its row unless another row holds the alias and the home plugin does not own it
func mirrorAliases(ctx context.Context, row, pp *Profile) {
	reg := providerRegistry()
	for _, alias := range pp.Aliases {
		if !reg.contains(ctx, alias) || userOwnsAlias(ctx, pp, alias) {
			reg.set(alias, row)
		}
	}
}

// A home plugin re-registering a name with a row wins for display name, base url and api key env vars
func overrideRegistryRow(row *Profile, pp Profile) {
	if pp.DisplayName != "" {
		row.DisplayName = pp.DisplayName
	}
	if pp.BaseURL != "" {
		row.BaseURL = pp.BaseURL
	}
	if profileRegistryAuthType(pp) == AuthTypeAPIKey && row.AuthType == AuthTypeAPIKey && len(pp.KeyEnv) > 0 {
		keyVars, urlVar := apiKeyEnvFields(pp)
		row.KeyEnv = keyVars
		if urlVar != "" {
			row.BaseURLEnv = urlVar
		}
	}
}

// Mirrors provider plugin profiles into the registry and returns how many were added
func syncPluginProviderRegistry(ctx context.Context) int {
	if ctx == nil {
		ctx = context.Background()
	}
	reg := providerRegistry()
	gid := goroutineID()
	_, outer := reg.syncing.Load(gid)
	if !outer {
		reg.syncing.Store(gid, true)
		defer reg.syncing.Delete(gid)
	}
	profiles := listProviders(ctx)
	homeProfile := func(name string) *Profile {
		user, _ := boundProfileResolver(ctx)(name)
		return user
	}
	added := 0
	for _, pp := range profiles {
		if reg.contains(ctx, pp.Name) {
			coreRow := reg.builtin[pp.Name] || pluginMirroredProvider(pp.Name)
			if coreRow && !registryPluginSkip[pp.Name] && ProviderSource(ctx, pp.Name) == "user" {
				row := reg.rawGet(pp.Name)
				if homeProfile(pp.Name) != pp {
					reg.mu.Lock()
					overrideRegistryRow(row, *pp)
					reg.mu.Unlock()
				}
				mirrorAliases(ctx, row, pp)
			}
			continue
		}
		registerPluginProvider(ctx, pp)
		if reg.contains(ctx, pp.Name) {
			added++
		}
	}
	return added
}

// Registry rows in order minus the alias keys pointing at a plugin row
func canonicalRegistryRows(ctx context.Context) []*Profile {
	var out []*Profile
	for _, it := range providerRegistry().items(ctx) {
		if it.Key == it.Row.Name {
			out = append(out, it.Row)
		}
	}
	return out
}

// Registry row for an id, re-syncing plugin profiles once on a miss
func registryLookup(ctx context.Context, providerID string) *Profile {
	if ctx == nil {
		ctx = context.Background()
	}
	reg := providerRegistry()
	row := reg.get(ctx, providerID)
	if row == nil {
		syncPluginProviderRegistry(ctx)
		row = reg.get(ctx, providerID)
	}
	return row
}

// Registry row merged with the registered profile of the same provider so profile hooks travel with it
func mergedRegistryProfile(ctx context.Context, row *Profile) Profile {
	user, shared := boundProfileResolver(ctx)(row.Name)
	prof := user
	if prof == nil {
		prof = shared
	}
	if prof == nil {
		return *row
	}
	out := *prof
	out.Name = row.Name
	out.DisplayName = row.DisplayName
	out.AuthType = row.AuthType
	out.Auth = row.Auth
	out.BaseURL = row.BaseURL
	out.KeyEnv = row.KeyEnv
	out.BaseURLEnv = row.BaseURLEnv
	return out
}
