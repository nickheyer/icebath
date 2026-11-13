package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

type frozenRuntimeConfigKey struct{}

// WithFrozenRuntimeConfig scopes resolution under the returned context to a session's frozen config over the defaults
func WithFrozenRuntimeConfig(ctx context.Context, cfg map[string]any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	merged := config.DeepMerge(map[string]any(config.Defaults()), cfg)
	return context.WithValue(ctx, frozenRuntimeConfigKey{}, merged)
}

// FrozenRuntimeConfig is the frozen session config scoped on the context, nil outside a frozen scope
func FrozenRuntimeConfig(ctx context.Context) map[string]any {
	if ctx == nil {
		return nil
	}
	cfg, _ := ctx.Value(frozenRuntimeConfigKey{}).(map[string]any)
	return cfg
}

// LoadConfigSafe is the context's frozen runtime config else the home's read only config, nil on any error
func LoadConfigSafe(ctx context.Context, h string) map[string]any {
	if frozen := FrozenRuntimeConfig(ctx); frozen != nil {
		return frozen
	}
	return loadLiveConfigSafe(h)
}

// Read only config of the home or the process home, nil on any error
func loadLiveConfigSafe(h string) map[string]any {
	if h == "" {
		h = home.Process()
	}
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil
	}
	return map[string]any(tree)
}

// ConfigLoader binds the home's read only config to a PoolEnv, whose context applies a frozen scope first
func ConfigLoader(h string) func() map[string]any {
	return func() map[string]any { return loadLiveConfigSafe(h) }
}

// ---- custom endpoint pool keys ----

// GetCustomProviderPoolKey is the preferred pool key for a custom provider, durable slug else custom name
func GetCustomProviderPoolKey(baseURL, providerName string, cfg map[string]any) string {
	candidates := customProviderPoolKeyCandidates(baseURL, providerName, cfg)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

// ListCustomPoolProviders lists every custom prefixed pool key that holds rows in auth.json
func ListCustomPoolProviders(store *Store) []string {
	pools, err := store.ReadCredentialPools()
	if err != nil {
		return nil
	}
	var keys []string
	for key, rows := range pools {
		if strings.HasPrefix(key, CustomPoolPrefix) && len(rows) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// The custom_providers config entry matching a custom prefixed pool key
func getCustomProviderConfig(poolKey string, cfg map[string]any) map[string]any {
	if !strings.HasPrefix(poolKey, CustomPoolPrefix) {
		return nil
	}
	suffix := poolKey[len(CustomPoolPrefix):]
	for _, ne := range iterCustomProviders(cfg) {
		if ne.name == suffix {
			return ne.entry
		}
	}
	return nil
}

// GetPoolStrategy is the configured selection strategy for a provider
func GetPoolStrategy(cfg map[string]any, provider string) string {
	strategies, _ := cfg["credential_pool_strategies"].(map[string]any)
	if strategies == nil {
		return StrategyFillFirst
	}
	strategy := strings.ToLower(strings.TrimSpace(anyString(strategies[provider])))
	if SupportedPoolStrategies[strategy] {
		return strategy
	}
	return StrategyFillFirst
}

// Matches a durable providers key pool against runtime identities
func keyedCustomPoolMatches(poolProvider, providerNorm, baseURL string, cfg map[string]any) bool {
	runtimeURL := normPoolURL(baseURL)
	if runtimeURL == "" {
		return false
	}
	for _, ne := range iterCustomProviders(cfg) {
		providerKey := normalizeCustomPoolName(cfgString(ne.entry["provider_key"]))
		if providerKey != poolProvider {
			continue
		}
		aliases := customEntryNameAliases(ne.name, ne.entry)
		aliases[CustomPoolPrefix+ne.name] = true
		if providerKey != "" {
			aliases[CustomPoolPrefix+providerKey] = true
		}
		configuredURL := normPoolURL(ne.entry["base_url"])
		if providerNorm == "custom" {
			return runtimeURL == configuredURL
		}
		return intersects(requestedCustomNameAliases(providerNorm), aliases) && runtimeURL == configuredURL
	}
	return false
}

func intersects(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// Matches a legacy custom prefixed pool against a named runtime identity
func legacyCustomPoolMatches(poolProvider, providerNorm, runtimeURL string, cfg map[string]any) bool {
	for _, ne := range iterCustomProviders(cfg) {
		if CustomPoolPrefix+ne.name != poolProvider {
			continue
		}
		aliases := map[string]bool{ne.name: true}
		for _, v := range []any{ne.entry["name"], ne.entry["provider_key"]} {
			if alias := normalizeCustomPoolName(cfgString(v)); alias != "" {
				aliases[alias] = true
				if strings.HasPrefix(alias, CustomPoolPrefix) {
					aliases[alias[len(CustomPoolPrefix):]] = true
				}
			}
		}
		configuredURL := normPoolURL(ne.entry["base_url"])
		runtimeAliases := map[string]bool{normalizeCustomPoolName(providerNorm): true}
		if strings.HasPrefix(providerNorm, CustomPoolPrefix) {
			runtimeAliases[normalizeCustomPoolName(providerNorm[len(CustomPoolPrefix):])] = true
		}
		return intersects(runtimeAliases, aliases) && runtimeURL == configuredURL
	}
	return false
}

// CredentialPoolMatchesProviderIn reports whether a pool key belongs to the runtime provider under cfg
func CredentialPoolMatchesProviderIn(poolProvider, provider, baseURL string, cfg map[string]any) bool {
	poolProvider = strings.ToLower(strings.TrimSpace(poolProvider))
	providerNorm := strings.ToLower(strings.TrimSpace(provider))
	if poolProvider == "" || providerNorm == "" {
		return false
	}
	if !strings.HasPrefix(poolProvider, CustomPoolPrefix) {
		if poolProvider == providerNorm {
			return true
		}
		return keyedCustomPoolMatches(poolProvider, providerNorm, baseURL, cfg)
	}
	if providerNorm == "custom" {
		if strings.ToLower(strings.TrimSpace(GetCustomProviderPoolKey(baseURL, "", cfg))) == poolProvider {
			return true
		}
		for _, key := range customProviderPoolKeyCandidates(baseURL, "", cfg) {
			if strings.ToLower(strings.TrimSpace(key)) == poolProvider {
				return true
			}
		}
		return false
	}
	runtimeURL := normPoolURL(baseURL)
	if runtimeURL == "" {
		return false
	}
	return legacyCustomPoolMatches(poolProvider, providerNorm, runtimeURL, cfg)
}

// CredentialPoolMatchesProvider is CredentialPoolMatchesProviderIn under the process config
func CredentialPoolMatchesProvider(ctx context.Context, poolProvider, provider, baseURL string) bool {
	return CredentialPoolMatchesProviderIn(poolProvider, provider, baseURL, LoadConfigSafe(ctx, ""))
}

// ResolveRuntimePoolKey is the pool key for a runtime provider identity, the normalized identity when no scoped key fits
func ResolveRuntimePoolKey(provider, baseURL string, cfg map[string]any) string {
	providerNorm := strings.ToLower(strings.TrimSpace(provider))
	if providerNorm == "" {
		return ""
	}
	accepts := func(candidate string) bool {
		return CredentialPoolMatchesProviderIn(candidate, providerNorm, baseURL, cfg)
	}
	if providerNorm == "custom" {
		if candidate := GetCustomProviderPoolKey(baseURL, "", cfg); candidate != "" && accepts(candidate) {
			return strings.ToLower(strings.TrimSpace(candidate))
		}
		return providerNorm
	}
	for _, ne := range iterCustomProviders(cfg) {
		for _, candidate := range poolKeysForCustomEntry(ne.name, ne.entry) {
			if accepts(candidate) {
				return candidate
			}
		}
	}
	return providerNorm
}

// ---- multi profile root write through ----

// Applies the test seat belt to the global root path, never the real user's root under a test run
func guardedGlobalRoot(globalPath string) string {
	if globalPath == "" {
		return ""
	}
	if os.Getenv("PYTEST_CURRENT_TEST") != "" || os.Getenv("ICEBATH_TEST_GUARD_ROOT") != "" {
		if realHome := os.Getenv("HOME"); realHome != "" {
			realRoot := filepath.Join(realHome, ".icebath", "auth.json")
			if samePath(globalPath, realRoot) {
				return ""
			}
		}
	}
	return globalPath
}

// Persists a rotated OAuth state into the global root auth.json, failures only logged
func writeThroughProviderStateToGlobalRoot(env PoolEnv, providerID string, state map[string]any) {
	globalPath := guardedGlobalRoot(env.Store.globalPath)
	if globalPath == "" {
		return
	}
	if _, err := env.Store.PersistProviderStateToStore(providerID, state, globalPath, false); err != nil {
		env.log().Debug(fmt.Sprintf("%s pool refresh: write-through to global root failed: %v", providerID, err))
	}
}

// True when an already loaded store has its own rows for the provider
func storeOwnsPoolProvider(data AuthData, provider string) bool {
	return len(poolEntries(poolMap(data)[provider])) > 0
}

// True when the active auth.json has its own rows for the provider
func profileOwnsPoolProvider(env PoolEnv, provider string) bool {
	if env.Store.globalPath == "" {
		return true
	}
	data, err := env.Store.Load()
	if err != nil {
		return true
	}
	return storeOwnsPoolProvider(data, provider)
}

// Global root auth.json when persisting a borrowed single use pool, empty to persist to the active store
func borrowedSingleUsePoolRoot(env PoolEnv) string {
	return guardedGlobalRoot(env.Store.globalPath)
}

// Update only merge of payloads into the root store's rows for the provider
func updateRootPoolRows(env PoolEnv, provider string, payloads []map[string]any, globalPath string, statusClearedIDs []string, tokenBases map[string]TokenPair) ([]map[string]any, error) {
	unlock, err := env.Store.fileLock(globalPath, AuthLockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := env.Store.LoadFile(globalPath)
	if err != nil {
		return nil, err
	}
	pool, ok := data["credential_pool"].(map[string]any)
	if !ok {
		pool = map[string]any{}
		data["credential_pool"] = pool
	}
	existing := poolEntries(pool[provider])
	incoming := entryIDs(payloads)
	cleared := map[string]bool{}
	for _, id := range statusClearedIDs {
		if id != "" {
			cleared[id] = true
		}
	}
	merged := make([]map[string]any, 0, len(existing))
	changed := false
	for _, disk := range existing {
		did := entryID(disk)
		in, present := incoming[did]
		if did == "" || !present {
			merged = append(merged, disk)
			continue
		}
		var base *TokenPair
		if pair, has := tokenBases[did]; has {
			base = &pair
		}
		updated := mergePoolRowGeneration(in, disk, provider, base, cleared[did])
		if !sameAnyMap(updated, disk) {
			changed = true
		}
		merged = append(merged, updated)
	}
	if changed {
		rows := make([]any, 0, len(merged))
		for _, m := range merged {
			rows = append(rows, m)
		}
		pool[provider] = rows
		if err := env.Store.SaveTo(data, globalPath); err != nil {
			return nil, err
		}
	}
	return merged, nil
}

// PersistPoolEntries writes a provider's rows to the store that owns them, false when a borrowed write failed
func PersistPoolEntries(env PoolEnv, provider string, payloads []map[string]any, removedIDs, statusClearedIDs []string, tokenBases map[string]TokenPair) ([]map[string]any, bool) {
	if SingleUseRefreshPoolProviders[provider] && !profileOwnsPoolProvider(env, provider) {
		if globalPath := borrowedSingleUsePoolRoot(env); globalPath != "" {
			written, err := updateRootPoolRows(env, provider, payloads, globalPath, statusClearedIDs, tokenBases)
			if err != nil {
				env.log().Warn(fmt.Sprintf("%s pool: write-through of borrowed root grant failed (%v); not materializing a profile-local copy", provider, err))
				return nil, false
			}
			return written, true
		}
	}
	written, err := env.Store.WriteCredentialPool(provider, payloads, removedIDs, statusClearedIDs, tokenBases)
	if err != nil {
		env.log().Warn(fmt.Sprintf("%s pool: persist failed (%v)", provider, err))
		return nil, false
	}
	return written, true
}

// True when a profile that owns its own pool rows would seed providers state from root
func ownedProfileReadsRootState(env PoolEnv, data AuthData, providerID, sourcePath string) bool {
	globalRoot := env.Store.globalPath
	return sourcePath != "" && globalRoot != "" && samePath(sourcePath, globalRoot) && storeOwnsPoolProvider(data, providerID)
}

// ---- seeding ----

// Upserts a payload for one source, dropping duplicate rows of that source
func upsertEntry(entries *[]PooledCredential, provider, source string, payload map[string]any, scope *SecretScope) bool {
	var matching []int
	for i, e := range *entries {
		if e.Source == source {
			matching = append(matching, i)
		}
	}
	duplicates := map[int]bool{}
	for _, idx := range matching[min(len(matching), 1):] {
		duplicates[idx] = true
	}
	if len(duplicates) > 0 {
		kept := (*entries)[:0:0]
		for i, e := range *entries {
			if !duplicates[i] {
				kept = append(kept, e)
			}
		}
		*entries = kept
		matching = matching[:1]
		for i, e := range *entries {
			if e.Source == source {
				matching[0] = i
				break
			}
		}
	}
	if len(matching) == 0 {
		if _, ok := payload["id"]; !ok {
			payload["id"] = newPoolID()
		}
		if _, ok := payload["priority"]; !ok {
			payload["priority"] = nextPriority(*entries)
		}
		if anyString(payload["label"]) == "" {
			payload["label"] = source
		}
		row := PooledCredentialFromMap(provider, payload)
		row.scope = scope
		*entries = append(*entries, row)
		return true
	}
	idx := matching[0]
	existing := (*entries)[idx]
	updated := existing.Clone()
	changed := false
	incomingToken, hasToken := payload["access_token"]
	tokenChanged := hasToken && incomingToken != nil && anyString(incomingToken) != existing.AccessToken
	if tokenChanged && existing.AccessToken == "" {
		if known := anyString(existing.Extra["secret_fingerprint"]); known != "" {
			tokenChanged = FingerprintSecretValue(incomingToken) != known
		}
	}
	for key, value := range payload {
		if key == "id" || key == "priority" || value == nil || (key == "label" && existing.Label != "") {
			continue
		}
		if poolFieldNames[key] {
			if applyPoolField(&updated, key, value) {
				changed = true
			}
		} else if poolExtraKeys[key] && !sameAnyValue(existing.Extra[key], value) {
			updated.Extra[key] = value
			changed = true
		}
	}
	if tokenChanged && existing.LastStatus != "" {
		updated = updated.ClearStatus()
		changed = true
	}
	if changed {
		(*entries)[idx] = updated
		return len(duplicates) > 0 || !sameAnyMap(existing.ToMap(), updated.ToMap())
	}
	return len(duplicates) > 0
}

func sameAnyValue(a, b any) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// Raw field values of the row keyed like the stored payload, secrets included
func poolFieldValues(e PooledCredential) map[string]any {
	return map[string]any{
		"label": e.Label, "auth_type": e.AuthType, "source": e.Source, "access_token": e.AccessToken, "refresh_token": e.RefreshToken,
		"last_status": e.LastStatus, "last_status_at": e.LastStatusAt, "last_error_code": e.LastErrorCode, "last_error_reason": e.LastErrorReason,
		"last_error_message": e.LastErrorMessage, "last_error_reset_at": e.LastErrorResetAt, "status_cleared_at": e.StatusClearedAt,
		"base_url": e.BaseURL, "expires_at": e.ExpiresAt, "expires_at_ms": e.ExpiresAtMS, "last_refresh": e.LastRefresh,
		"inference_base_url": e.InferenceBaseURL, "agent_key": e.AgentKey, "agent_key_expires_at": e.AgentKeyExpiresAt,
		"request_count": e.RequestCount, "model_cooldowns": e.ModelCooldowns,
	}
}

// Applies one field payload onto the row and reports whether the attribute changed
func applyPoolField(e *PooledCredential, key string, value any) bool {
	before := poolFieldValues(*e)
	switch key {
	case "label":
		e.Label = anyString(value)
	case "auth_type":
		e.AuthType = anyString(value)
	case "source":
		e.Source = anyString(value)
	case "access_token":
		e.AccessToken = anyString(value)
	case "refresh_token":
		e.RefreshToken = anyString(value)
	case "last_status":
		e.LastStatus = anyString(value)
	case "last_status_at":
		e.LastStatusAt, _ = ParseAbsoluteTimestamp(value)
	case "last_error_code":
		e.LastErrorCode, _ = anyInt(value)
	case "last_error_reason":
		e.LastErrorReason = anyString(value)
	case "last_error_message":
		e.LastErrorMessage = anyString(value)
	case "last_error_reset_at":
		e.LastErrorResetAt, _ = anyFloat(value)
	case "status_cleared_at":
		e.StatusClearedAt, _ = anyFloat(value)
	case "base_url":
		e.BaseURL = anyString(value)
	case "expires_at":
		e.ExpiresAt = anyString(value)
	case "expires_at_ms":
		if f, ok := anyFloat(value); ok {
			e.ExpiresAtMS = int64(f)
		}
	case "last_refresh":
		e.LastRefresh = anyString(value)
	case "inference_base_url":
		e.InferenceBaseURL = anyString(value)
	case "agent_key":
		e.AgentKey = anyString(value)
	case "agent_key_expires_at":
		e.AgentKeyExpiresAt = anyString(value)
	case "request_count":
		e.RequestCount, _ = anyInt(value)
	case "model_cooldowns":
		e.ModelCooldowns = cooldownsFromAny(value)
	default:
		return false
	}
	return !sameAnyValue(before[key], poolFieldValues(*e)[key])
}

var anthropicSourceRank = map[string]int{"env:ANTHROPIC_TOKEN": 0, "env:CLAUDE_CODE_OAUTH_TOKEN": 1, "hermes_pkce": 2, "claude_code": 3, "env:ANTHROPIC_API_KEY": 4}

// Orders anthropic rows manual first then by seeded source rank, reporting whether priorities changed
func normalizePoolPriorities(provider string, entries []PooledCredential) bool {
	if provider != "anthropic" {
		return false
	}
	var manual, seeded []PooledCredential
	for _, e := range entries {
		if isManualSource(e.Source) {
			manual = append(manual, e)
		} else {
			seeded = append(seeded, e)
		}
	}
	sort.SliceStable(manual, func(i, j int) bool { return manual[i].Priority < manual[j].Priority })
	rank := func(e PooledCredential) int {
		if r, ok := anthropicSourceRank[e.Source]; ok {
			return r
		}
		return len(anthropicSourceRank)
	}
	sort.SliceStable(seeded, func(i, j int) bool {
		a, b := seeded[i], seeded[j]
		if rank(a) != rank(b) {
			return rank(a) < rank(b)
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.Label < b.Label
	})
	index := map[string]int{}
	for i, e := range entries {
		index[e.ID] = i
	}
	changed := false
	for newPriority, e := range append(manual, seeded...) {
		if e.Priority != newPriority {
			entries[index[e.ID]].Priority = newPriority
			changed = true
		}
	}
	return changed
}

// Removes entries whose source is in drop, true when anything was removed
func retainSourcesNotIn(entries *[]PooledCredential, drop map[string]bool) bool {
	kept := (*entries)[:0:0]
	for _, e := range *entries {
		if !drop[e.Source] {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(*entries) {
		return false
	}
	*entries = kept
	return true
}

// Accumulates upsert results for one load seeding pass
type seeder struct {
	env           PoolEnv
	provider      string
	entries       *[]PooledCredential
	changed       bool
	activeSources map[string]bool
	suppressed    map[string]bool
}

func newSeeder(env PoolEnv, provider string, entries *[]PooledCredential) *seeder {
	return &seeder{env: env, provider: provider, entries: entries, activeSources: map[string]bool{},
		suppressed: env.Store.SuppressedCredentialSources(provider)}
}

func (s *seeder) isSuppressed(source string) bool { return s.suppressed[source] }

// Upserts unless the source is suppressed so a removal stays stable across loads
func (s *seeder) upsert(source string, payload map[string]any) bool {
	if s.isSuppressed(source) {
		return false
	}
	s.activeSources[source] = true
	payload["source"] = source
	ingested := upsertEntry(s.entries, s.provider, source, payload, s.env.Scope)
	s.changed = s.changed || ingested
	return ingested
}

// Value from the profile .env then the scope, blank when neither defines it
func (env PoolEnv) envVal(key string) string {
	if v := strings.TrimSpace(env.envFile()[key]); v != "" {
		return v
	}
	return strings.TrimSpace(env.secretStr(key))
}

func seedAnthropicSingletons(seed *seeder) {
	if !IsProviderExplicitlyConfigured(seed.env, "anthropic") {
		return
	}
	oauthEnv := seed.env.envVal("ANTHROPIC_TOKEN")
	if oauthEnv == "" {
		oauthEnv = seed.env.envVal("CLAUDE_CODE_OAUTH_TOKEN")
	}
	if seed.env.envVal("ANTHROPIC_API_KEY") != "" && oauthEnv == "" {
		seed.changed = retainSourcesNotIn(seed.entries, map[string]bool{"hermes_pkce": true, "claude_code": true}) || seed.changed
		return
	}
	type src struct {
		name  string
		creds map[string]any
	}
	sources := []src{{"hermes_pkce", ReadHermesOAuthCredentials(seed.env)}}
	if AdoptExternalLoginsEnabled(seed.env) {
		sources = append(sources, src{"claude_code", ReadClaudeCodeCredentials(seed.env)})
	} else {
		seed.changed = retainSourcesNotIn(seed.entries, map[string]bool{"claude_code": true}) || seed.changed
	}
	for _, s := range sources {
		access := anyString(s.creds["accessToken"])
		if s.creds == nil || access == "" {
			continue
		}
		payload := map[string]any{"auth_type": AuthTypeOAuth, "access_token": access, "label": LabelFromToken(access, s.name)}
		if rt := anyString(s.creds["refreshToken"]); rt != "" {
			payload["refresh_token"] = rt
		}
		if ms, ok := anyFloat(s.creds["expiresAt"]); ok {
			payload["expires_at_ms"] = ms
		}
		seed.upsert(s.name, payload)
	}
}

func seedNousSingleton(seed *seeder, data AuthData) {
	state, sourcePath := seed.env.Store.ProviderStateWithSource(data, "nous")
	if ownedProfileReadsRootState(seed.env, data, "nous", sourcePath) {
		return
	}
	hasRuntimeMaterial := state != nil && !guestWithoutFreeTier(state) &&
		(strings.TrimSpace(anyString(state["access_token"])) != "" || strings.TrimSpace(anyString(state["agent_key"])) != "")
	if len(state) > 0 && !hasRuntimeMaterial {
		seed.changed = retainSourcesNotIn(seed.entries, map[string]bool{"device_code": true, "manual:device_code": true}) || seed.changed
	}
	if len(state) == 0 || !hasRuntimeMaterial {
		return
	}
	access := anyString(state["access_token"])
	payload := map[string]any{"auth_type": AuthTypeOAuth, "access_token": access}
	for _, key := range []string{"refresh_token", "expires_at", "token_type", "scope", "client_id", "portal_base_url", "inference_base_url", "agent_key", "agent_key_expires_at"} {
		if v, ok := state[key]; ok && v != nil {
			payload[key] = v
		}
	}
	for _, key := range nousExtraStateKeys {
		if v, ok := state[key]; ok && v != nil {
			payload[key] = v
		}
	}
	if tls, ok := state["tls"].(map[string]any); ok {
		payload["tls"] = tls
	}
	if label := strings.TrimSpace(anyString(state["label"])); label != "" {
		payload["label"] = label
	} else {
		payload["label"] = LabelFromToken(access, "device_code")
	}
	seed.upsert("device_code", payload)
}

// Rows a plugin provider profile seeds itself from ambient tokens it validates or exchanges
func seedProfileSingleton(seed *seeder) {
	hooks := providerProfileHooks(seed.env.ctx(), seed.provider)
	if hooks == nil || hooks.SeedPoolCredentials == nil {
		return
	}
	rows := hooks.SeedPoolCredentials(seed.env.Store.SuppressedCredentialSources(seed.provider), IsProviderExplicitlyConfigured(seed.env, seed.provider))
	pconfig, hasProfile := ProfileFor(seed.provider)
	for _, row := range rows {
		source := anyString(row["source"])
		baseURL := anyString(row["base_url"])
		if baseURL == "" && hasProfile {
			baseURL = pconfig.BaseURL
		}
		label := anyString(row["label"])
		if label == "" {
			label = source
		}
		seed.upsert(source, map[string]any{"auth_type": AuthTypeAPIKey, "access_token": anyString(row["access_token"]), "base_url": baseURL, "label": label})
	}
}

// Whether the provider's profile owns pool seeding so the env loop must not overwrite it
func profileSeedsPool(env PoolEnv, provider string) bool {
	hooks := providerProfileHooks(env.ctx(), provider)
	return hooks != nil && hooks.SeedPoolCredentials != nil
}

// Qwen CLI credential reader the seeder consults, swappable so tests can stand in for the CLI file
var qwenCredentialsForSeed = func(env PoolEnv, refreshIfExpiring bool) (map[string]any, error) {
	return resolveQwenRuntimeCredentials(env, refreshIfExpiring)
}

func seedQwenSingleton(seed *seeder) {
	creds, err := qwenCredentialsForSeed(seed.env, false)
	if err != nil {
		seed.env.log().Debug(fmt.Sprintf("Qwen OAuth token seed failed: %v", err))
		return
	}
	token := anyString(creds["api_key"])
	if token == "" {
		return
	}
	source := anyString(creds["source"])
	if source == "" {
		source = "qwen-cli"
	}
	label := anyString(creds["auth_file"])
	if label == "" {
		label = source
	}
	payload := map[string]any{"auth_type": AuthTypeOAuth, "access_token": token, "base_url": anyString(creds["base_url"]), "label": label}
	if ms, ok := anyFloat(creds["expires_at_ms"]); ok {
		payload["expires_at_ms"] = ms
	}
	seed.upsert(source, payload)
}

func seedMinimaxSingleton(seed *seeder) {
	state, err := seed.env.Store.GetProviderAuthState("minimax-oauth")
	if err != nil {
		seed.env.log().Debug(fmt.Sprintf("MiniMax OAuth token seed failed: %v", err))
		return
	}
	access := anyString(state["access_token"])
	if state == nil || access == "" {
		return
	}
	payload := map[string]any{"auth_type": AuthTypeOAuth, "access_token": access,
		"base_url": strings.TrimRight(anyString(state["inference_base_url"]), "/")}
	if rt := anyString(state["refresh_token"]); rt != "" {
		payload["refresh_token"] = rt
	}
	if raw := anyString(state["expires_at"]); raw != "" {
		if epoch, ok := ParseISOTimestamp(raw); ok {
			payload["expires_at_ms"] = float64(int64(epoch * 1000))
		}
	}
	if label := anyString(state["label"]); label != "" {
		payload["label"] = label
	} else {
		payload["label"] = LabelFromToken(access, "oauth")
	}
	seed.upsert("oauth", payload)
}

// Surfaces the Codex or xAI auth.json tokens singleton as a device_code row
func seedTokensSingleton(seed *seeder, data AuthData) {
	state, sourcePath := seed.env.Store.ProviderStateWithSource(data, seed.provider)
	if ownedProfileReadsRootState(seed.env, data, seed.provider, sourcePath) {
		return
	}
	tokens, ok := state["tokens"].(map[string]any)
	access := anyString(tokens["access_token"])
	if !ok || access == "" {
		return
	}
	baseURL, customLabel := DefaultXAIOAuthBaseURL, ""
	if seed.provider == "openai-codex" {
		baseURL = DefaultCodexBaseURL
		customLabel = strings.TrimSpace(anyString(state["label"]))
	}
	payload := map[string]any{"auth_type": AuthTypeOAuth, "access_token": access, "base_url": baseURL}
	if rt := anyString(tokens["refresh_token"]); rt != "" {
		payload["refresh_token"] = rt
	}
	if lr := anyString(state["last_refresh"]); lr != "" {
		payload["last_refresh"] = lr
	}
	if customLabel != "" {
		payload["label"] = customLabel
	} else {
		payload["label"] = LabelFromToken(access, "device_code")
	}
	seed.upsert("device_code", payload)
}

func seedFromSingletons(env PoolEnv, provider string, entries *[]PooledCredential) (bool, map[string]bool) {
	seed := newSeeder(env, provider, entries)
	data, err := env.Store.Load()
	if err != nil {
		data = emptyAuthData()
	}
	switch {
	case provider == "anthropic":
		seedAnthropicSingletons(seed)
	case provider == "nous":
		seedNousSingleton(seed, data)
	case provider == "qwen-oauth":
		seedQwenSingleton(seed)
	case provider == "minimax-oauth":
		seedMinimaxSingleton(seed)
	case tokensSingletonProviders[provider].refresh != nil:
		if provider == "openai-codex" && seed.isSuppressed("device_code") {
			return seed.changed, seed.activeSources
		}
		seedTokensSingleton(seed, data)
	default:
		seedProfileSingleton(seed)
	}
	return seed.changed, seed.activeSources
}

// GetEnvPreferDotenv resolves a credential env var preferring the profile .env over the scope
func GetEnvPreferDotenv(env PoolEnv, key string) string {
	raw := strings.TrimSpace(env.envFile()[key])
	scoped := strings.TrimSpace(env.secretStr(key))
	if strings.HasPrefix(raw, "op://") && scoped != "" {
		return scoped
	}
	if raw != "" {
		return raw
	}
	return scoped
}

var (
	envIngestionWarnedMu sync.Mutex
	envIngestionWarned   = map[string]bool{}
)

// Warns once per process per provider when an env credential is ingested into a paid pool
func warnEnvIngestionOnce(env PoolEnv, provider, envVar string) {
	envIngestionWarnedMu.Lock()
	defer envIngestionWarnedMu.Unlock()
	if envIngestionWarned[provider] {
		return
	}
	envIngestionWarned[provider] = true
	spend := provider
	if provider == "openrouter" {
		spend = "OpenRouter"
	}
	env.log().Warn(fmt.Sprintf("Ingested %s from environment into the %s credential pool — this enables %s spend. Remove the key or run icebath auth remove %s <n> to suppress.", envVar, provider, spend, provider))
}

func envPayload(env PoolEnv, envVar, token, baseURL string) map[string]any {
	payload := map[string]any{"auth_type": AuthTypeAPIKey, "access_token": token, "base_url": baseURL, "label": envVar}
	if source := strings.TrimSpace(SecretSourceOf(env.Store.Home(), envVar)); source != "" {
		payload["secret_source"] = source
	}
	return payload
}

// Declared env vars, their numbered siblings and the env sources already persisted
func envKeyVarCandidates(env PoolEnv, envVars []string, entries []PooledCredential) []string {
	names := append([]string(nil), envVars...)
	for _, base := range envVars {
		for n := 2; GetEnvPreferDotenv(env, fmt.Sprintf("%s_%d", base, n)) != ""; n++ {
			names = append(names, fmt.Sprintf("%s_%d", base, n))
		}
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Source, "env:") {
			name := strings.TrimSpace(e.Source[len("env:"):])
			if name != "" && !containsString(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func seedFromEnv(env PoolEnv, provider string, entries *[]PooledCredential) (bool, map[string]bool) {
	seed := newSeeder(env, provider, entries)
	if profileSeedsPool(env, provider) {
		return seed.changed, seed.activeSources
	}
	if provider == "openrouter" {
		for _, envVar := range envKeyVarCandidates(env, []string{"OPENROUTER_API_KEY"}, *entries) {
			token := GetEnvPreferDotenv(env, envVar)
			if token != "" && seed.upsert("env:"+envVar, envPayload(env, envVar, token, OpenRouterBaseURL)) {
				warnEnvIngestionOnce(env, provider, envVar)
			}
		}
		return seed.changed, seed.activeSources
	}
	pconfig, ok := ProfileFor(provider)
	if !ok || pconfig.AuthType != AuthTypeAPIKey {
		return seed.changed, seed.activeSources
	}
	envURL := ""
	if pconfig.BaseURLEnv != "" {
		envURL = strings.TrimRight(GetEnvPreferDotenv(env, pconfig.BaseURLEnv), "/")
	}
	envVars := append([]string(nil), pconfig.KeyEnv...)
	if provider == "anthropic" {
		envVars = []string{"ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}
	}
	for _, envVar := range envKeyVarCandidates(env, envVars, *entries) {
		token := GetEnvPreferDotenv(env, envVar)
		if token == "" {
			continue
		}
		baseURL := envURL
		if baseURL == "" {
			baseURL = pconfig.BaseURL
		}
		switch provider {
		case "kimi-coding":
			baseURL = resolveKimiBaseURL(token, pconfig.BaseURL, envURL)
		case "zai":
			if envURL != "" {
				baseURL = envURL
			} else {
				baseURL = resolveZaiBaseURLDetected(env.ctx(), env.Store.Home(), token, pconfig.BaseURL)
			}
		}
		seed.upsert("env:"+envVar, envPayload(env, envVar, token, baseURL))
	}
	return seed.changed, seed.activeSources
}

// Drops seeded rows whose backing source is gone, env rows only when explicitly requested
func pruneStaleSeededEntries(entries *[]PooledCredential, activeSources map[string]bool, pruneEnvSources bool) bool {
	prunable := func(e PooledCredential) bool {
		if strings.HasPrefix(e.Source, "env:") {
			return pruneEnvSources
		}
		return IsBorrowedCredentialSource(e.Source, e.Provider) || e.Source == "hermes_pkce"
	}
	kept := (*entries)[:0:0]
	for _, e := range *entries {
		if isManualSource(e.Source) || activeSources[e.Source] || !prunable(e) {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(*entries) {
		return false
	}
	*entries = kept
	return true
}

// Seeds a custom endpoint pool from custom provider config and the model config key
func seedCustomPool(env PoolEnv, poolKey string, entries *[]PooledCredential) (bool, map[string]bool) {
	seed := newSeeder(env, poolKey, entries)
	cfg := env.config()
	if cp := getCustomProviderConfig(poolKey, cfg); cp != nil {
		apiKey := strings.TrimSpace(anyString(cp["api_key"]))
		name := strings.TrimSpace(anyString(cp["name"]))
		if apiKey != "" {
			label := name
			if label == "" {
				label = "config:" + name
			}
			seed.upsert("config:"+name, map[string]any{"auth_type": AuthTypeAPIKey, "access_token": apiKey, "base_url": normPoolURL(cp["base_url"]), "label": label})
		}
	}
	if modelCfg, ok := cfg["model"].(map[string]any); ok {
		modelProvider := strings.ToLower(strings.TrimSpace(anyString(modelCfg["provider"])))
		modelBaseURL := normPoolURL(modelCfg["base_url"])
		modelAPIKey := ""
		for _, k := range []string{"api_key", "api"} {
			if v, ok := modelCfg[k].(string); ok && strings.TrimSpace(v) != "" {
				modelAPIKey = strings.TrimSpace(v)
				break
			}
		}
		if modelProvider == "custom" && modelBaseURL != "" && modelAPIKey != "" {
			for _, key := range customProviderPoolKeyCandidates(modelBaseURL, "", cfg) {
				if strings.ToLower(strings.TrimSpace(key)) == poolKey {
					seed.upsert("model_config", map[string]any{"auth_type": AuthTypeAPIKey, "access_token": modelAPIKey, "base_url": modelBaseURL, "label": "model_config"})
					break
				}
			}
		}
	}
	return seed.changed, seed.activeSources
}

// LoadPool reads a provider's rows, seeds them from singletons env and config, and persists changes
func LoadPool(env PoolEnv, provider string) *Pool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if SingleUseRefreshPoolProviders[provider] {
		healForkedSingleUseOAuthGrants(env, provider)
	}
	raw, err := env.Store.ReadCredentialPool(provider)
	if err != nil {
		env.log().Warn(fmt.Sprintf("%s pool: auth store unreadable (%v); loading an empty pool", provider, err))
		raw = nil
	}
	diskIDs := map[string]bool{}
	for _, row := range raw {
		if id := entryID(row); id != "" {
			diskIDs[id] = true
		}
	}
	changed := false
	for _, row := range raw {
		if !sameAnyMap(SanitizeBorrowedCredentialPayload(row, provider), row) {
			changed = true
		}
	}
	entries := make([]PooledCredential, 0, len(raw))
	for _, row := range raw {
		e := PooledCredentialFromMap(provider, row)
		e.scope = env.Scope
		entries = append(entries, e)
	}
	needsAuthNormalization := false
	for _, row := range raw {
		authType := anyString(row["auth_type"])
		if _, ok := row["auth_type"]; !ok {
			authType = AuthTypeAPIKey
		}
		if normalizePoolAuthType(provider, anyString(row["access_token"]), authType) != authType {
			needsAuthNormalization = true
			break
		}
	}
	if needsAuthNormalization {
		if data, err := env.Store.Load(); err == nil && storeOwnsPoolProvider(data, provider) {
			changed = true
		}
	}
	var ownsProvider, ownsKnown bool
	union := func(a, b map[string]bool) map[string]bool {
		out := map[string]bool{}
		for k := range a {
			out[k] = true
		}
		for k := range b {
			out[k] = true
		}
		return out
	}
	if strings.HasPrefix(provider, CustomPoolPrefix) {
		customChanged, customSources := seedCustomPool(env, provider, &entries)
		changed = changed || customChanged
		changed = pruneStaleSeededEntries(&entries, customSources, true) || changed
	} else {
		singletonChanged, singletonSources := seedFromSingletons(env, provider, &entries)
		envChanged, envSources := seedFromEnv(env, provider, &entries)
		changed = changed || singletonChanged || envChanged
		active := union(singletonSources, envSources)
		if SingleUseRefreshPoolProviders[provider] && len(diskIDs) > 0 {
			ownsProvider, ownsKnown = profileOwnsPoolProvider(env, provider), true
		}
		if ownsKnown && !ownsProvider {
			var borrowed, others []PooledCredential
			for _, e := range entries {
				if diskIDs[e.ID] {
					borrowed = append(borrowed, e)
				} else {
					others = append(others, e)
				}
			}
			changed = pruneStaleSeededEntries(&others, active, false) || changed
			entries = append(borrowed, others...)
		} else {
			changed = pruneStaleSeededEntries(&entries, active, false) || changed
		}
		changed = normalizePoolPriorities(provider, entries) || changed
	}
	pool := NewPool(env, provider, entries)
	pool.persistedTokenPairs = TokenPairsByID(raw)
	if changed {
		var removed []string
		live := map[string]bool{}
		for _, e := range entries {
			live[e.ID] = true
		}
		for id := range diskIDs {
			if !live[id] {
				removed = append(removed, id)
			}
		}
		sort.Strings(removed)
		pool.persist(false, removed, nil)
	}
	if SingleUseRefreshPoolProviders[provider] && len(diskIDs) > 0 {
		if changed {
			ownsProvider = profileOwnsPoolProvider(env, provider)
		}
		if !ownsProvider {
			pool.mu.Lock()
			for id := range diskIDs {
				pool.borrowedRootIDs[id] = true
			}
			pool.mu.Unlock()
		}
	}
	return pool
}

// ---- explicit configuration ----

var implicitEnvVars = map[string]bool{"CLAUDE_CODE_OAUTH_TOKEN": true}
var explicitPoolSources = map[string]bool{"device_code": true, "loopback_pkce": true, "hermes_pkce": true, "manual": true}
var vertexProviderIDs = map[string]bool{"vertex": true, "google-vertex": true, "vertex-ai": true, "gcp-vertex": true, "vertexai": true}

// True when the env var resolves to a usable secret in the active profile scope
func envSecretPresent(env PoolEnv, name string) bool {
	return hasUsableSecret(GetEnvPreferDotenv(env, name))
}

func slotSelects(slot any, normalized string) bool {
	m, ok := slot.(map[string]any)
	return ok && strings.ToLower(strings.TrimSpace(anyString(m["provider"]))) == normalized
}

// config.yaml model.provider, an auxiliary task provider or a MoA slot naming the provider
func configSelectsProvider(cfg map[string]any, normalized string) bool {
	if slotSelects(cfg["model"], normalized) {
		return true
	}
	if aux, ok := cfg["auxiliary"].(map[string]any); ok {
		for _, slot := range aux {
			if slotSelects(slot, normalized) {
				return true
			}
		}
	}
	moa, ok := cfg["moa"].(map[string]any)
	if !ok {
		return false
	}
	blockMatches := func(block any) bool {
		m, ok := block.(map[string]any)
		if !ok {
			return false
		}
		if refs, ok := m["reference_models"].([]any); ok {
			for _, s := range refs {
				if slotSelects(s, normalized) {
					return true
				}
			}
		}
		return slotSelects(m["aggregator"], normalized)
	}
	if blockMatches(moa) {
		return true
	}
	if presets, ok := moa["presets"].(map[string]any); ok {
		for _, p := range presets {
			if blockMatches(p) {
				return true
			}
		}
	}
	return false
}

// True when the user pasted an explicit credential env var for the provider
func explicitEnvCredentialsPresent(env PoolEnv, normalized string) bool {
	pconfig, ok := ProfileFor(normalized)
	if !ok {
		return false
	}
	switch pconfig.AuthType {
	case AuthTypeAPIKey:
		for _, v := range pconfig.KeyEnv {
			if !implicitEnvVars[v] && envSecretPresent(env, v) {
				return true
			}
		}
	case AuthTypeAWSSDK:
		return envSecretPresent(env, "AWS_BEARER_TOKEN_BEDROCK") || (envSecretPresent(env, "AWS_ACCESS_KEY_ID") && envSecretPresent(env, "AWS_SECRET_ACCESS_KEY"))
	}
	return false
}

// True for pool rows the user created through an explicit flow or a still live env key
func poolEntryIsExplicit(env PoolEnv, row map[string]any) bool {
	source := strings.ToLower(strings.TrimSpace(anyString(row["source"])))
	if strings.HasPrefix(source, "env:") {
		envVar := strings.TrimSpace(anyString(row["source"])[len("env:"):])
		return envVar != "" && envSecretPresent(env, envVar)
	}
	return source != "" && (explicitPoolSources[source] || strings.HasPrefix(source, "manual:"))
}

// Vertex and Bedrock count as explicit only through icebath scoped routing config
func keylessProviderHasExplicitConfig(env PoolEnv, normalized string) bool {
	if vertexProviderIDs[normalized] {
		return hasExplicitVertexConfig(env)
	}
	if normalized == "bedrock" {
		bedrock, ok := env.config()["bedrock"].(map[string]any)
		return ok && strings.TrimSpace(anyString(bedrock["region"])) != ""
	}
	return false
}

// IsProviderExplicitlyConfigured is true only when the user configured the provider through an explicit flow
func IsProviderExplicitlyConfigured(env PoolEnv, providerID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(providerID))
	if data, err := env.Store.Load(); err == nil {
		active := strings.ToLower(strings.TrimSpace(anyString(data["active_provider"])))
		if active != "" && active == normalized {
			return true
		}
	}
	if configSelectsProvider(env.config(), normalized) {
		return true
	}
	if explicitEnvCredentialsPresent(env, normalized) {
		return true
	}
	if rows, err := env.Store.ReadCredentialPool(normalized); err == nil {
		for _, row := range rows {
			if poolEntryIsExplicit(env, row) {
				return true
			}
		}
	}
	return keylessProviderHasExplicitConfig(env, normalized)
}

// PoolCredentials is the credential source the resolver reads pooled logins through
type PoolCredentials struct {
	env   PoolEnv
	mu    sync.Mutex
	pools map[string]*Pool
}

// NewPoolCredentials builds a pool backed credential source over one home
func NewPoolCredentials(env PoolEnv) *PoolCredentials {
	return &PoolCredentials{env: env, pools: map[string]*Pool{}}
}

// Pool loads and caches the pool for a key
func (c *PoolCredentials) Pool(poolKey string) *Pool {
	key := strings.ToLower(strings.TrimSpace(poolKey))
	if key == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if pool, ok := c.pools[key]; ok {
		return pool
	}
	pool := LoadPool(c.env, key)
	c.pools[key] = pool
	return pool
}

// Reload drops every cached pool so the next read reloads from disk
func (c *PoolCredentials) Reload() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pools = map[string]*Pool{}
}

// Credential is the pooled login for a provider key
func (c *PoolCredentials) Credential(provider string) (Credential, bool) {
	return c.CredentialForModel(provider, "")
}

// CredentialForModel is the pooled login not benched for model
func (c *PoolCredentials) CredentialForModel(provider, model string) (Credential, bool) {
	pool := c.Pool(provider)
	if pool == nil || !pool.HasCredentials() {
		return Credential{}, false
	}
	entry, ok := pool.Select(model)
	if !ok {
		return Credential{}, false
	}
	return poolCredential(pool, entry), true
}

// TokenBenchedForModel reports a model cooldown the provider's pool holds for the token
func (c *PoolCredentials) TokenBenchedForModel(provider, token, model string) bool {
	pool := c.Pool(provider)
	return pool != nil && pool.TokenIsBlocked(token, model)
}

// CredentialForEntry narrows the pool to the rows a custom entry owns before selecting
func (c *PoolCredentials) CredentialForEntry(poolKey string, entry map[string]any) (Credential, bool) {
	pool := EntryScopedPool(c.Pool(poolKey), poolKey, entry)
	if pool == nil || !pool.HasCredentials() {
		return Credential{}, false
	}
	row, ok := pool.Select("")
	if !ok {
		return Credential{}, false
	}
	return poolCredential(pool, row), true
}

// LoadPoolProbe adapts LoadPool to the chain's PoolProbe loader
func (c *PoolCredentials) LoadPoolProbe(provider string) PoolProbe {
	pool := c.Pool(provider)
	if pool == nil {
		return nil
	}
	return pool
}
