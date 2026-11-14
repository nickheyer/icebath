package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

const openRouterBaseURL = "https://openrouter.ai/api/v1"

var entryExtraKeys = []string{
	"token_type", "scope", "client_id", "portal_base_url", "obtained_at",
	"expires_in", "agent_key_id", "agent_key_expires_in", "agent_key_reused",
	"agent_key_obtained_at", "tls", "secret_source", "secret_fingerprint",
	"auth_method", "account_tier", "anon_token", "user_id", "org_id",
	"failure_reason",
}

func upsertEntry(entries *[]*Entry, provider, source string, payload *doc) bool {
	existingIdx := -1
	duplicates := false
	kept := []*Entry{}
	for _, e := range *entries {
		if e.Source == source {
			if existingIdx >= 0 {
				duplicates = true
				continue
			}
			existingIdx = len(kept)
		}
		kept = append(kept, e)
	}
	*entries = kept
	if existingIdx < 0 {
		if !payload.has("id") {
			payload.set("id", shortID())
		}
		if !payload.has("priority") {
			payload.set("priority", intNum(int64(nextPriority(kept))))
		}
		if !payload.has("label") {
			payload.set("label", source)
		}
		*entries = append(kept, entryFromDoc(provider, payload))
		return true
	}
	existing := kept[existingIdx]
	before := existing.rawDoc()
	after := before.clone()
	incoming, hasToken := payload.value("access_token").(string)
	tokenChanged := hasToken && incoming != existing.AccessToken.Reveal()
	if tokenChanged && existing.AccessToken.Empty() {
		if known := existing.extraText("secret_fingerprint"); known != "" {
			tokenChanged = FingerprintSecret(incoming) != known
		}
	}
	for _, key := range payload.keys {
		value := payload.vals[key]
		if key == "id" || key == "priority" || value == nil || (key == "label" && existing.Label != "") {
			continue
		}
		if slices.Contains(entryFieldNames, key) || slices.Contains(entryExtraKeys, key) {
			after.set(key, value)
		}
	}
	if tokenChanged && existing.LastStatus != "" {
		for _, f := range []string{"last_status", "last_status_at", "last_error_code", "last_error_reason", "last_error_message", "last_error_reset_at"} {
			after.set(f, nil)
		}
	}
	if valueEqual(before, after) {
		return duplicates
	}
	updated := entryFromDoc(provider, after)
	updated.rt = existing.rt
	kept[existingIdx] = updated
	return duplicates || !valueEqual(existing.toDoc(), updated.toDoc())
}

var anthropicSourceRank = map[string]int{
	"env:ANTHROPIC_TOKEN": 0, "env:CLAUDE_CODE_OAUTH_TOKEN": 1, sourceIcebathPKCE: 2, sourceClaudeCode: 3, "env:ANTHROPIC_API_KEY": 4,
}

func normalizePoolPriorities(provider string, entries *[]*Entry) bool {
	if provider != ProviderAnthropic {
		return false
	}
	manual, seeded := []*Entry{}, []*Entry{}
	for _, e := range *entries {
		if isManualSource(e.Source) {
			manual = append(manual, e)
		} else {
			seeded = append(seeded, e)
		}
	}
	rank := func(e *Entry) int {
		if r, ok := anthropicSourceRank[e.Source]; ok {
			return r
		}
		return len(anthropicSourceRank)
	}
	sort.SliceStable(manual, func(i, j int) bool { return manual[i].Priority < manual[j].Priority })
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
	changed := false
	for priority, e := range append(manual, seeded...) {
		if e.Priority == priority {
			continue
		}
		for i, current := range *entries {
			if current.ID == e.ID {
				u := current.clone()
				u.Priority = priority
				(*entries)[i] = u
			}
		}
		changed = true
	}
	return changed
}

func retainSourcesNotIn(entries *[]*Entry, drop ...string) bool {
	retained := slices.DeleteFunc(slices.Clone(*entries), func(e *Entry) bool { return slices.Contains(drop, e.Source) })
	if len(retained) == len(*entries) {
		return false
	}
	*entries = retained
	return true
}

type seeder struct {
	s        *Store
	ctx      context.Context
	provider string
	entries  *[]*Entry
	changed  bool
	active   []string
}

func (sd *seeder) upsert(source string, payload *doc) bool {
	if sd.s.IsSuppressed(sd.ctx, sd.provider, source) {
		return false
	}
	sd.active = append(sd.active, source)
	payload.set("source", source)
	ingested := upsertEntry(sd.entries, sd.provider, source, payload)
	sd.changed = sd.changed || ingested
	return ingested
}

func seedPayload(pairs ...any) *doc {
	d := newDoc()
	for i := 0; i+1 < len(pairs); i += 2 {
		key, _ := pairs[i].(string)
		switch v := pairs[i+1].(type) {
		case string:
			if v == "" && key != "access_token" && key != "base_url" && key != "label" {
				d.set(key, nil)
			} else {
				d.set(key, v)
			}
		case int64:
			if v == 0 {
				d.set(key, nil)
			} else {
				d.set(key, intNum(v))
			}
		default:
			d.set(key, v)
		}
	}
	return d
}

func (s *Store) envPreferFile(ctx context.Context, key string) string {
	v, err := s.preferFile(ctx, key)
	if err != nil {
		slog.Debug("auth: environment read refused", "name", key, "error", err)
		return ""
	}
	return strings.TrimSpace(v)
}

func (sd *seeder) seedAnthropic() error {
	explicit, err := sd.s.IsExplicitlyConfigured(sd.ctx, ProviderAnthropic)
	if err != nil {
		return err
	}
	if !explicit {
		return nil
	}
	oauthEnv := sd.s.envPreferFile(sd.ctx, "ANTHROPIC_TOKEN")
	if oauthEnv == "" {
		oauthEnv = sd.s.envPreferFile(sd.ctx, "CLAUDE_CODE_OAUTH_TOKEN")
	}
	if sd.s.envPreferFile(sd.ctx, "ANTHROPIC_API_KEY") != "" && oauthEnv == "" {
		sd.changed = retainSourcesNotIn(sd.entries, sourceIcebathPKCE, sourceClaudeCode) || sd.changed
		return nil
	}
	own := sd.s.readIcebathOAuthCredentials()
	var claude *claudeCreds
	if sd.s.adoptExternalLogins(sd.ctx) {
		claude = sd.s.readClaudeCodeCredentials(sd.ctx)
	} else {
		sd.changed = retainSourcesNotIn(sd.entries, sourceClaudeCode) || sd.changed
	}
	if access := textOf(own.value("accessToken")); own != nil && access != "" {
		expires, _ := intOf(own.value("expiresAt"))
		sd.upsert(sourceIcebathPKCE, seedPayload("auth_type", AuthTypeOAuth, "access_token", access, "refresh_token", textOf(own.value("refreshToken")),
			"expires_at_ms", expires, "label", LabelFromToken(access, sourceIcebathPKCE)))
	}
	if claude != nil && claude.accessToken != "" {
		sd.upsert(sourceClaudeCode, seedPayload("auth_type", AuthTypeOAuth, "access_token", claude.accessToken, "refresh_token", claude.refreshToken,
			"expires_at_ms", claude.expiresAt, "label", LabelFromToken(claude.accessToken, sourceClaudeCode)))
	}
	return nil
}

func (sd *seeder) seedPortal(store *doc) {
	state, source := sd.s.stateWithSource(store, ProviderPortal)
	if sd.s.ownedProfileReadsRootState(store, ProviderPortal, source) {
		return
	}
	seedable := sd.spec().StateSeedable == nil || sd.spec().StateSeedable(rawOf(state))
	material := seedable && state != nil && (stripped(state.value("access_token")) != "" || stripped(state.value("agent_key")) != "")
	if state.size() > 0 && !material {
		sd.changed = retainSourcesNotIn(sd.entries, "device_code", SourceManualDeviceCode) || sd.changed
	}
	if state.size() == 0 || !material {
		return
	}
	payload := newDoc()
	payload.set("auth_type", AuthTypeOAuth)
	payload.set("access_token", textOf(state.value("access_token")))
	for _, k := range []string{"refresh_token", "expires_at", "token_type", "scope", "client_id", "portal_base_url", "inference_base_url", "agent_key", "agent_key_expires_at"} {
		payload.set(k, state.value(k))
	}
	for _, k := range portalExtraStateKeys {
		payload.set(k, state.value(k))
	}
	payload.set("tls", nil)
	if tls := state.obj("tls"); tls != nil {
		payload.set("tls", tls)
	}
	label := stripped(state.value("label"))
	if label == "" {
		label = LabelFromToken(textOf(state.value("access_token")), "device_code")
	}
	payload.set("label", label)
	sd.upsert("device_code", payload)
}

func (sd *seeder) spec() SeedSpec {
	spec, _ := sd.s.dir.Spec(sd.ctx, sd.provider)
	return spec
}

func (sd *seeder) seedProfile() error {
	spec := sd.spec()
	if spec.SeedPool == nil {
		return nil
	}
	suppressed := sd.s.Suppressed(sd.ctx, sd.provider)
	explicit, err := sd.s.IsExplicitlyConfigured(sd.ctx, sd.provider)
	if err != nil {
		return err
	}
	for _, row := range spec.SeedPool(sd.ctx, suppressed, explicit) {
		baseURL, label := row.BaseURL, row.Label
		if baseURL == "" {
			baseURL = spec.BaseURL
		}
		if label == "" {
			label = row.Source
		}
		sd.upsert(row.Source, seedPayload("auth_type", AuthTypeAPIKey, "access_token", row.AccessToken.Reveal(), "base_url", baseURL, "label", label))
	}
	return nil
}

func (sd *seeder) seedQwen() {
	creds, err := sd.s.ResolveQwen(sd.ctx, ResolveOptions{NoRefresh: true})
	if err != nil {
		slog.Debug("Qwen OAuth token seed failed", "error", err)
		return
	}
	if creds.APIKey.Empty() {
		return
	}
	sd.upsert(creds.Source, seedPayload("auth_type", AuthTypeOAuth, "access_token", creds.APIKey.Reveal(), "expires_at_ms", creds.ExpiresAtMS,
		"base_url", creds.BaseURL, "label", creds.AuthFile))
}

func (sd *seeder) seedMiniMax() {
	state, err := sd.s.providerState(sd.ctx, ProviderMiniMax)
	if err != nil || !truthy(state.value("access_token")) {
		return
	}
	expiresMS := int64(0)
	if t, ok := parseISO(state.str("expires_at"), time.Local); ok {
		expiresMS = int64(epochOf(t) * 1000)
	}
	access := textOf(state.value("access_token"))
	label := textOf(state.value("label"))
	if label == "" {
		label = LabelFromToken(access, "oauth")
	}
	sd.upsert("oauth", seedPayload("auth_type", AuthTypeOAuth, "access_token", access, "refresh_token", textOf(state.value("refresh_token")),
		"expires_at_ms", expiresMS, "base_url", strings.TrimRight(textOf(state.value("inference_base_url")), "/"), "label", label))
}

func (sd *seeder) seedTokens(store *doc) {
	state, source := sd.s.stateWithSource(store, sd.provider)
	if sd.s.ownedProfileReadsRootState(store, sd.provider, source) {
		return
	}
	tokens := state.obj("tokens")
	if !truthy(tokens.value("access_token")) {
		return
	}
	baseURL, label := DefaultXAIOAuthBaseURL, ""
	if sd.provider == ProviderCodex {
		baseURL, label = DefaultCodexBaseURL, stripped(state.value("label"))
	}
	access := textOf(tokens.value("access_token"))
	if label == "" {
		label = LabelFromToken(access, "device_code")
	}
	sd.upsert("device_code", seedPayload("auth_type", AuthTypeOAuth, "access_token", access, "refresh_token", textOf(tokens.value("refresh_token")),
		"base_url", baseURL, "last_refresh", textOf(state.value("last_refresh")), "label", label))
}

func (s *Store) seedFromSingletons(ctx context.Context, provider string, entries *[]*Entry) (bool, []string, error) {
	sd := &seeder{s: s, ctx: ctx, provider: provider, entries: entries}
	store, err := s.load(s.authPath)
	if err != nil {
		return false, nil, err
	}
	switch {
	case provider == ProviderAnthropic:
		err = sd.seedAnthropic()
	case provider == ProviderPortal:
		sd.seedPortal(store)
	case provider == ProviderQwen:
		sd.seedQwen()
	case provider == ProviderMiniMax:
		sd.seedMiniMax()
	case isTokensSingleton(provider):
		if provider == ProviderCodex && s.IsSuppressed(ctx, provider, "device_code") {
			break
		}
		sd.seedTokens(store)
	default:
		err = sd.seedProfile()
	}
	if err != nil {
		return false, nil, err
	}
	return sd.changed, sd.active, nil
}

func (s *Store) warnEnvIngestionOnce(provider, envVar string) {
	s.mu.Lock()
	warned := slices.Contains(s.envIngestionWarned, provider)
	if !warned {
		s.envIngestionWarned = append(s.envIngestionWarned, provider)
	}
	s.mu.Unlock()
	if warned {
		return
	}
	spend := provider
	if provider == ProviderOpenRoute {
		spend = "OpenRouter"
	}
	slog.Warn(fmt.Sprintf("Ingested %s from environment into the %s credential pool — this enables %s spend. "+
		"Remove the key or run icebath auth remove %s <n> to suppress.", envVar, provider, spend, provider))
}

func (s *Store) envPayload(envVar, token, baseURL string) *doc {
	payload := seedPayload("auth_type", AuthTypeAPIKey, "access_token", token, "base_url", baseURL, "label", envVar)
	if source, ok := s.env.SecretSource(envVar); ok && strings.TrimSpace(source) != "" {
		payload.set("secret_source", strings.TrimSpace(source))
	}
	return payload
}

func (s *Store) envKeyVarCandidates(ctx context.Context, envVars []string, entries []*Entry) []string {
	names := slices.Clone(envVars)
	for _, base := range envVars {
		for n := 2; s.envPreferFile(ctx, fmt.Sprintf("%s_%d", base, n)) != ""; n++ {
			names = append(names, fmt.Sprintf("%s_%d", base, n))
		}
	}
	for _, e := range entries {
		if name, ok := strings.CutPrefix(e.Source, "env:"); ok {
			if name = strings.TrimSpace(name); name != "" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func (s *Store) seedFromEnv(ctx context.Context, provider string, entries *[]*Entry) (bool, []string) {
	sd := &seeder{s: s, ctx: ctx, provider: provider, entries: entries}
	spec, known := s.dir.Spec(ctx, provider)
	if spec.SeedPool != nil {
		return false, nil
	}
	if provider == ProviderOpenRoute {
		for _, envVar := range s.envKeyVarCandidates(ctx, []string{"OPENROUTER_API_KEY"}, *entries) {
			if token := s.envPreferFile(ctx, envVar); token != "" && sd.upsert("env:"+envVar, s.envPayload(envVar, token, openRouterBaseURL)) {
				s.warnEnvIngestionOnce(provider, envVar)
			}
		}
		return sd.changed, sd.active
	}
	if !known || spec.AuthType != AuthTypeAPIKey {
		return false, nil
	}
	envURL := ""
	if spec.BaseURLEnv != "" {
		envURL = strings.TrimRight(s.envPreferFile(ctx, spec.BaseURLEnv), "/")
	}
	envVars := slices.Clone(spec.EnvVars)
	if provider == ProviderAnthropic {
		envVars = []string{"ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}
	}
	for _, envVar := range s.envKeyVarCandidates(ctx, envVars, *entries) {
		token := s.envPreferFile(ctx, envVar)
		if token == "" {
			continue
		}
		baseURL := envURL
		if baseURL == "" {
			baseURL = spec.BaseURL
		}
		if (provider == "kimi-coding" || provider == "zai") && spec.RuntimeBaseURL != nil {
			if owned, ok := spec.RuntimeBaseURL(ctx, NewSecret(token), spec.BaseURL, envURL); ok {
				baseURL = owned
			}
		}
		sd.upsert("env:"+envVar, s.envPayload(envVar, token, baseURL))
	}
	return sd.changed, sd.active
}

func pruneStaleSeededEntries(entries *[]*Entry, active []string, pruneEnv bool) bool {
	retained := slices.DeleteFunc(slices.Clone(*entries), func(e *Entry) bool {
		if isManualSource(e.Source) || slices.Contains(active, e.Source) {
			return false
		}
		if strings.HasPrefix(e.Source, "env:") {
			return pruneEnv
		}
		return IsBorrowedSource(e.Source, e.Provider) || e.Source == sourceIcebathPKCE
	})
	if len(retained) == len(*entries) {
		return false
	}
	*entries = retained
	return true
}

func normalizeCustomPoolName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "-")
}

func normURL(v string) string { return strings.TrimRight(strings.TrimSpace(v), "/") }

type CustomEntry struct {
	Name               string
	ProviderKey        string
	BaseURL            string
	APIKey             Secret
	KeyEnv             string
	APIKeyEnv          string
	KeyCmd             string
	DisplayName        string
	DeclaresCredential bool
}

func customEntryOf(c config.CustomEndpoint) CustomEntry {
	e := CustomEntry{
		Name: normalizeCustomPoolName(c.Text("name")), DisplayName: c.Text("name"), ProviderKey: c.Text("provider_key"), BaseURL: c.Text("base_url"),
		APIKey: NewSecret(c.Text("api_key")), KeyEnv: c.Text("key_env"), APIKeyEnv: c.Text("api_key_env"), KeyCmd: c.Text("key_cmd"),
	}
	e.DeclaresCredential = strings.TrimSpace(e.APIKey.Reveal()) != "" || strings.TrimSpace(e.KeyEnv) != "" || strings.TrimSpace(e.APIKeyEnv) != "" || strings.TrimSpace(e.KeyCmd) != ""
	return e
}

func (s *Store) customEntries(ctx context.Context, frozen []CustomEntry) []CustomEntry {
	if frozen != nil {
		return frozen
	}
	endpoints, err := s.cfg.CustomEndpoints(ctx)
	if err != nil {
		return nil
	}
	out := []CustomEntry{}
	for _, c := range endpoints {
		if c.Text("name") != "" {
			out = append(out, customEntryOf(c))
		}
	}
	return out
}

func (s *Store) CustomEntries(ctx context.Context) []CustomEntry {
	return s.customEntries(s.bind(ctx), nil)
}

func customEntryNameAliases(e CustomEntry) []string {
	aliases := []string{e.Name}
	if key := normalizeCustomPoolName(e.ProviderKey); key != "" && key != e.Name {
		aliases = append(aliases, key)
	}
	return aliases
}

func requestedCustomNameAliases(name string) []string {
	normalized := normalizeCustomPoolName(name)
	aliases := []string{}
	if normalized != "" {
		aliases = append(aliases, normalized)
	}
	if suffix, ok := strings.CutPrefix(normalized, CustomPoolPrefix); ok {
		if suffix = normalizeCustomPoolName(suffix); suffix != "" {
			aliases = append(aliases, suffix)
		}
	}
	return aliases
}

func intersects(a, b []string) bool {
	return slices.ContainsFunc(a, func(v string) bool { return slices.Contains(b, v) })
}

func poolKeysForCustomEntry(e CustomEntry) []string {
	keys := []string{}
	legacy := ""
	if e.Name != "" {
		legacy = CustomPoolPrefix + e.Name
	}
	for _, key := range []string{normalizeCustomPoolName(e.ProviderKey), legacy} {
		if normalized := strings.ToLower(strings.TrimSpace(key)); normalized != "" && !slices.Contains(keys, normalized) {
			keys = append(keys, normalized)
		}
	}
	return keys
}

func (s *Store) namedCustomEntry(ctx context.Context, providerName string, frozen []CustomEntry) (CustomEntry, bool) {
	aliases := requestedCustomNameAliases(providerName)
	if len(aliases) == 0 {
		return CustomEntry{}, false
	}
	for _, e := range s.customEntries(ctx, frozen) {
		if intersects(aliases, customEntryNameAliases(e)) {
			return e, true
		}
	}
	return CustomEntry{}, false
}

func (s *Store) CustomEntryForEndpoint(ctx context.Context, baseURL, providerName string, frozen []CustomEntry) (CustomEntry, bool) {
	ctx = s.bind(ctx)
	if baseURL == "" {
		return CustomEntry{}, false
	}
	target := normURL(baseURL)
	if named, ok := s.namedCustomEntry(ctx, providerName, frozen); ok {
		if entryURL := normURL(named.BaseURL); entryURL != "" && entryURL != target {
			return CustomEntry{}, false
		}
		return named, true
	}
	for _, e := range s.customEntries(ctx, frozen) {
		if target != "" && normURL(e.BaseURL) == target {
			return e, true
		}
	}
	return CustomEntry{}, false
}

func (s *Store) CustomPoolKeyCandidates(ctx context.Context, baseURL, providerName string, frozen []CustomEntry) []string {
	match, ok := s.CustomEntryForEndpoint(ctx, baseURL, providerName, frozen)
	if !ok {
		return nil
	}
	return poolKeysForCustomEntry(match)
}

func (s *Store) CustomPoolKey(ctx context.Context, baseURL, providerName string) (string, bool) {
	candidates := s.CustomPoolKeyCandidates(ctx, baseURL, providerName, nil)
	if len(candidates) == 0 {
		return "", false
	}
	return candidates[0], true
}

func (s *Store) ListCustomPoolProviders(ctx context.Context) ([]string, error) {
	all, err := s.readPoolAll(ctx)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for _, key := range all.keys {
		if strings.HasPrefix(key, CustomPoolPrefix) && len(all.list(key)) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *Store) PoolMatchesProvider(ctx context.Context, poolProvider, provider, baseURL string) bool {
	ctx = s.bind(ctx)
	pool := strings.ToLower(strings.TrimSpace(poolProvider))
	want := strings.ToLower(strings.TrimSpace(provider))
	if pool == "" || want == "" {
		return false
	}
	runtimeURL := normURL(baseURL)
	if !strings.HasPrefix(pool, CustomPoolPrefix) {
		if pool == want {
			return true
		}
		if runtimeURL == "" {
			return false
		}
		for _, e := range s.customEntries(ctx, nil) {
			key := normalizeCustomPoolName(e.ProviderKey)
			if key != pool {
				continue
			}
			aliases := append(customEntryNameAliases(e), CustomPoolPrefix+e.Name, CustomPoolPrefix+key)
			if want == "custom" {
				return runtimeURL == normURL(e.BaseURL)
			}
			return intersects(requestedCustomNameAliases(want), aliases) && runtimeURL == normURL(e.BaseURL)
		}
		return false
	}
	if want == "custom" {
		return slices.Contains(s.CustomPoolKeyCandidates(ctx, baseURL, "", nil), pool)
	}
	if runtimeURL == "" {
		return false
	}
	for _, e := range s.customEntries(ctx, nil) {
		if CustomPoolPrefix+e.Name != pool {
			continue
		}
		aliases := []string{e.Name}
		for _, value := range []string{e.DisplayName, e.ProviderKey} {
			if alias := normalizeCustomPoolName(value); alias != "" {
				aliases = append(aliases, alias, strings.TrimPrefix(alias, CustomPoolPrefix))
			}
		}
		requested := []string{normalizeCustomPoolName(want)}
		if suffix, ok := strings.CutPrefix(want, CustomPoolPrefix); ok {
			requested = append(requested, normalizeCustomPoolName(suffix))
		}
		return intersects(requested, aliases) && runtimeURL == normURL(e.BaseURL)
	}
	return false
}

func (s *Store) ResolveRuntimePoolKey(ctx context.Context, provider, baseURL string) string {
	ctx = s.bind(ctx)
	want := strings.ToLower(strings.TrimSpace(provider))
	if want == "" {
		return ""
	}
	if want == "custom" {
		if candidate, ok := s.CustomPoolKey(ctx, baseURL, ""); ok && s.PoolMatchesProvider(ctx, candidate, want, baseURL) {
			return strings.ToLower(strings.TrimSpace(candidate))
		}
		return want
	}
	for _, e := range s.customEntries(ctx, nil) {
		for _, candidate := range poolKeysForCustomEntry(e) {
			if s.PoolMatchesProvider(ctx, candidate, want, baseURL) {
				return candidate
			}
		}
	}
	return want
}

func EntryServesEndpoint(ctx context.Context, e Entry, baseURL string, normalize func(string) string) bool {
	if baseURL == "" {
		return true
	}
	entryURL := e.RuntimeBaseURL(ctx)
	if entryURL == "" {
		entryURL = e.BaseURL
	}
	if entryURL == "" {
		return true
	}
	return normalize(entryURL) == normalize(baseURL)
}

func rowOwnedByEntry(row *Entry, entry CustomEntry) bool {
	source := strings.TrimSpace(row.Source)
	if isManualSource(source) {
		return true
	}
	for _, name := range []string{strings.TrimSpace(entry.KeyEnv), strings.TrimSpace(entry.APIKeyEnv)} {
		if name != "" && source == "env:"+name {
			return true
		}
	}
	return false
}

func (s *Store) EntryScopedPool(ctx context.Context, pool *Pool, poolKey string, entry *CustomEntry) *Pool {
	if pool == nil || strings.HasPrefix(poolKey, CustomPoolPrefix) || entry == nil || !entry.DeclaresCredential {
		return pool
	}
	pool.mu.Lock()
	rows := slices.Clone(pool.entries)
	pairs := pool.persistedPairs
	pool.mu.Unlock()
	owned := slices.DeleteFunc(slices.Clone(rows), func(e *Entry) bool { return !rowOwnedByEntry(e, *entry) })
	if len(owned) == len(rows) {
		return pool
	}
	if len(owned) == 0 {
		return nil
	}
	view := s.newPool(s.bind(ctx), pool.provider, owned)
	view.persistedPairs = pairs
	return view
}

func (s *Store) PoolForAuxProvider(ctx context.Context, provider, poolKey string) (*Pool, error) {
	pool, err := s.Pool(ctx, poolKey)
	if err != nil || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(provider)), CustomPoolPrefix) {
		return pool, err
	}
	if match, ok := s.namedCustomEntry(s.bind(ctx), provider, nil); ok {
		return s.EntryScopedPool(ctx, pool, poolKey, &match), nil
	}
	return pool, nil
}

func customIdentity(provider, requested string) (custom bool, name string) {
	for _, v := range []string{strings.ToLower(strings.TrimSpace(provider)), strings.ToLower(strings.TrimSpace(requested))} {
		if v == "custom" || strings.HasPrefix(v, CustomPoolPrefix) {
			custom = true
		}
		if name == "" && strings.HasPrefix(v, CustomPoolPrefix) {
			name = v
		}
	}
	return custom, name
}

func (s *Store) RuntimeEntryOwner(ctx context.Context, provider, requested, baseURL string) (CustomEntry, bool) {
	custom, name := customIdentity(provider, requested)
	if !custom {
		return CustomEntry{}, false
	}
	match, ok := s.CustomEntryForEndpoint(ctx, baseURL, name, nil)
	if !ok {
		return CustomEntry{}, false
	}
	match.APIKey, match.KeyCmd = Secret{}, ""
	return match, true
}

func (s *Store) LoadRuntimePool(ctx context.Context, provider, baseURL, requested string, restoring bool, owner *CustomEntry) (*Pool, error) {
	ctx = s.bind(ctx)
	custom, name := customIdentity(provider, requested)
	if !custom {
		key := s.ResolveRuntimePoolKey(ctx, provider, baseURL)
		if key == "" {
			return nil, nil
		}
		return s.Pool(ctx, key)
	}
	if restoring && name != "" && owner == nil {
		return nil, nil
	}
	var match *CustomEntry
	if owner != nil {
		match = owner
	} else if found, ok := s.CustomEntryForEndpoint(ctx, baseURL, name, nil); ok {
		match = &found
	}
	key := ""
	if match != nil && (name != "" || owner != nil) {
		if keys := poolKeysForCustomEntry(*match); len(keys) > 0 {
			key = keys[0]
		}
	} else {
		key = s.ResolveRuntimePoolKey(ctx, provider, baseURL)
	}
	if key == "" {
		return nil, nil
	}
	pool, err := s.Pool(ctx, key)
	if err != nil {
		return nil, err
	}
	return s.EntryScopedPool(ctx, pool, key, match), nil
}

func (s *Store) seedCustomPool(ctx context.Context, poolKey string, entries *[]*Entry) (bool, []string) {
	sd := &seeder{s: s, ctx: ctx, provider: poolKey, entries: entries}
	suffix := strings.TrimPrefix(poolKey, CustomPoolPrefix)
	for _, e := range s.customEntries(ctx, nil) {
		if e.Name != suffix {
			continue
		}
		if key := strings.TrimSpace(e.APIKey.Reveal()); key != "" {
			name := strings.TrimSpace(e.DisplayName)
			label := name
			if label == "" {
				label = "config:" + name
			}
			sd.upsert("config:"+name, seedPayload("auth_type", AuthTypeAPIKey, "access_token", key, "base_url", normURL(e.BaseURL), "label", label))
		}
		break
	}
	if cfg, err := s.cfg.LoadReadonly(ctx); err == nil && !cfg.Model.Bare {
		modelKey := strings.TrimSpace(cfg.Model.APIKey)
		if modelKey == "" {
			modelKey = strings.TrimSpace(cfg.Model.API)
		}
		modelURL := normURL(cfg.Model.BaseURL)
		if strings.ToLower(strings.TrimSpace(cfg.Model.Provider)) == "custom" && modelURL != "" && modelKey != "" {
			if slices.Contains(s.CustomPoolKeyCandidates(ctx, modelURL, "", nil), poolKey) {
				sd.upsert("model_config", seedPayload("auth_type", AuthTypeAPIKey, "access_token", modelKey, "base_url", modelURL, "label", "model_config"))
			}
		}
	}
	return sd.changed, sd.active
}

func (s *Store) profileOwnsPoolProvider(provider string) bool {
	if s.rootAuth == "" {
		return true
	}
	store, err := s.load(s.authPath)
	if err != nil {
		return true
	}
	return len(store.obj("credential_pool").list(provider)) > 0
}

func (s *Store) updateRootPoolRows(ctx context.Context, provider string, payloads []*doc, w poolWrite) ([]*doc, error) {
	ctx, unlock, err := s.lock(ctx, s.rootAuth, LockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	store, err := s.load(s.rootAuth)
	if err != nil {
		return nil, err
	}
	pool := store.section("credential_pool")
	incoming := entryIDs(payloads)
	merged := []any{}
	rows := []*doc{}
	changed := false
	for _, v := range pool.list(provider) {
		disk := asRow(v)
		id := rowID(disk)
		update, ok := incoming[id]
		if disk == nil || id == "" || !ok {
			merged = append(merged, v)
			if disk != nil {
				rows = append(rows, disk)
			}
			continue
		}
		var base *tokenPair
		if pair, known := w.tokenBases[id]; known {
			base = &pair
		}
		updated := mergePoolRowGeneration(update, disk, provider, base, slices.Contains(w.statusCleared, id))
		if !valueEqual(updated, disk) {
			changed = true
		}
		merged = append(merged, updated)
		rows = append(rows, updated)
	}
	if changed {
		pool.set(provider, merged)
		if err := s.save(ctx, store, s.rootAuth); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func (s *Store) persistPoolEntries(ctx context.Context, provider string, payloads []*doc, w poolWrite) ([]*doc, bool, error) {
	if IsSingleUseRefreshProvider(provider) && !s.profileOwnsPoolProvider(provider) && s.rootAuth != "" {
		rows, err := s.updateRootPoolRows(ctx, provider, payloads, w)
		if err != nil {
			slog.Warn(fmt.Sprintf("%s pool: write-through of borrowed root grant failed (%v); not materializing a profile-local copy", provider, err))
			return nil, false, nil
		}
		return rows, true, nil
	}
	rows, err := s.writePool(ctx, provider, payloads, w)
	return rows, err == nil, err
}

func (s *Store) Pool(ctx context.Context, provider string) (*Pool, error) {
	ctx = s.bind(ctx)
	provider = strings.ToLower(strings.TrimSpace(provider))
	singleUse := IsSingleUseRefreshProvider(provider)
	if singleUse {
		s.HealForkedSingleUseGrants(ctx, provider)
	}
	raw, err := s.readPool(ctx, provider)
	if err != nil {
		return nil, err
	}
	diskIDs := []string{}
	changed := false
	needsAuthNormalization := false
	entries := make([]*Entry, 0, len(raw))
	for _, payload := range raw {
		if id := rowID(payload); id != "" {
			diskIDs = append(diskIDs, id)
		}
		if !valueEqual(sanitizeBorrowedPayload(payload, provider), payload) {
			changed = true
		}
		storedType, isText := payload.value("auth_type").(string)
		if !payload.has("auth_type") {
			storedType, isText = AuthTypeAPIKey, true
		}
		if !isText || normalizePoolAuthType(provider, payload.str("access_token"), storedType) != storedType {
			needsAuthNormalization = true
		}
		entries = append(entries, entryFromDoc(provider, payload))
	}
	if needsAuthNormalization {
		active, err := s.load(s.authPath)
		if err != nil {
			return nil, err
		}
		if truthy(active.obj("credential_pool").value(provider)) {
			changed = true
		}
	}
	owns, ownsKnown := false, false
	if strings.HasPrefix(provider, CustomPoolPrefix) {
		customChanged, sources := s.seedCustomPool(ctx, provider, &entries)
		changed = changed || customChanged
		changed = pruneStaleSeededEntries(&entries, sources, true) || changed
	} else {
		singletonChanged, singletonSources, err := s.seedFromSingletons(ctx, provider, &entries)
		if err != nil {
			return nil, err
		}
		envChanged, envSources := s.seedFromEnv(ctx, provider, &entries)
		changed = changed || singletonChanged || envChanged
		active := append(slices.Clone(singletonSources), envSources...)
		if singleUse && len(diskIDs) > 0 {
			owns, ownsKnown = s.profileOwnsPoolProvider(provider), true
		}
		if ownsKnown && !owns {
			borrowed, others := []*Entry{}, []*Entry{}
			for _, e := range entries {
				if slices.Contains(diskIDs, e.ID) {
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
		changed = normalizePoolPriorities(provider, &entries) || changed
	}
	pool := s.newPool(ctx, provider, entries)
	pool.persistedPairs = tokenPairsByID(raw)
	if changed {
		removed := slices.DeleteFunc(slices.Clone(diskIDs), func(id string) bool {
			return slices.ContainsFunc(entries, func(e *Entry) bool { return e.ID == id })
		})
		sort.Strings(removed)
		if err := pool.persist(ctx, removed, nil); err != nil {
			return nil, err
		}
	}
	if singleUse && len(diskIDs) > 0 {
		if changed || !ownsKnown {
			owns = s.profileOwnsPoolProvider(provider)
		}
		if !owns {
			pool.borrowedRootIDs = diskIDs
		}
	}
	return pool, nil
}

func (e *Entry) MarshalJSON() ([]byte, error) {
	view := e.toDoc()
	for _, k := range view.keys {
		if isSecretPayloadKey(k) && truthy(view.vals[k]) {
			view.set(k, redacted)
		}
	}
	return json.Marshal(view)
}
