package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// CodexQuotaProbeMinInterval throttles the live Codex quota probe per access token
const CodexQuotaProbeMinInterval = 300 * time.Second

// SingleUseRefreshPoolProviders rotate a single use refresh token on every refresh
var SingleUseRefreshPoolProviders = map[string]bool{"anthropic": true, "openai-codex": true, "xai-oauth": true, "nous": true}

type tokensSingletonSpec struct {
	display, short string
	refresh        func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error)
	terminal       func(err error) bool
}

// Providers whose OAuth singleton lives under providers.<id>.tokens in auth.json
var tokensSingletonProviders = map[string]tokensSingletonSpec{
	"openai-codex": {display: "Codex", short: "Codex", refresh: refreshCodexOAuthPure, terminal: isTerminalCodexOAuthRefreshError},
	"xai-oauth":    {display: "xAI OAuth", short: "xAI", refresh: refreshXAIOAuthPure, terminal: isTerminalXAIOAuthRefreshError},
}

// RefreshableOAuthProviders are the built in providers whose pooled OAuth rows the pool can refresh
var RefreshableOAuthProviders = map[string]bool{"anthropic": true, "nous": true, "openai-codex": true, "xai-oauth": true}

var singleUseRefreshProviders = map[string]bool{"openai-codex": true, "xai-oauth": true, "anthropic": true}

// Nous runtime credential resolver the refresh path calls, swappable so tests can stand in for the Portal
var nousRuntimeCredentialsFunc = func(env PoolEnv, forceRefresh bool, staleAccessToken string) (map[string]any, error) {
	return resolveNousRuntimeCredentials(env, forceRefresh, staleAccessToken)
}

// Lock free window between consecutive auth store holds in a deferred refresh sweep
const refreshSweepSpacing = 500 * time.Millisecond

var refreshTimeoutEnvVars = map[string]string{"openai-codex": "ICEBATH_CODEX_REFRESH_TIMEOUT_SECONDS", "xai-oauth": "ICEBATH_XAI_REFRESH_TIMEOUT_SECONDS"}

// Singleton seeded source whose exhausted or DEAD row a re-auth in another process may revive
var resyncSource = map[string]string{"anthropic": "claude_code", "nous": "device_code", "openai-codex": "device_code", "xai-oauth": "device_code"}

// PoolEnv is the per home dependency set a credential pool reads and writes through
type PoolEnv struct {
	Store   *Store
	Secrets *config.Secrets
	Scope   *SecretScope
	// Read only config, the session's frozen config when one is frozen
	Config func() map[string]any
	Log    *slog.Logger
	HTTP   *http.Client
	Ctx    context.Context
}

func (env PoolEnv) log() *slog.Logger {
	if env.Log != nil {
		return env.Log
	}
	return slog.Default()
}

func (env PoolEnv) ctx() context.Context {
	if env.Ctx != nil {
		return env.Ctx
	}
	return context.Background()
}

func (env PoolEnv) config() map[string]any {
	if frozen := FrozenRuntimeConfig(env.ctx()); frozen != nil {
		return frozen
	}
	if env.Config == nil {
		return nil
	}
	return env.Config()
}

func (env PoolEnv) scope() *SecretScope {
	if env.Scope != nil {
		return env.Scope
	}
	return SecretScopeFrom(env.ctx())
}

func (env PoolEnv) secretStr(name string) string { return env.scope().GetSecretStr(name, "") }

// The profile .env assignments
func (env PoolEnv) envFile() map[string]string {
	if env.Secrets == nil {
		return map[string]string{}
	}
	return env.Secrets.File()
}

// Pool is a persistent multi credential pool for one provider with same provider failover
type Pool struct {
	provider                string
	env                     PoolEnv
	mu                      sync.Mutex
	entries                 []PooledCredential
	currentID               string
	borrowedRootIDs         map[string]bool
	persistedTokenPairs     map[string]TokenPair
	strategy                string
	activeLeases            map[string]int
	maxConcurrent           int
	lastNoEntriesLogAt      time.Time
	unmatchedRotationStreak int
}

// NewPool builds a pool over entries sorted by priority
func NewPool(env PoolEnv, provider string, entries []PooledCredential) *Pool {
	sorted := append([]PooledCredential(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })
	for i := range sorted {
		sorted[i].scope = env.Scope
	}
	return &Pool{provider: provider, env: env, entries: sorted, borrowedRootIDs: map[string]bool{},
		persistedTokenPairs: map[string]TokenPair{}, strategy: GetPoolStrategy(env.config(), provider),
		activeLeases: map[string]int{}, maxConcurrent: DefaultMaxConcurrentPerCredential}
}

// Provider is the pool key this pool is scoped to
func (p *Pool) Provider() string { return p.provider }

// Env is the dependency set the pool was loaded with
func (p *Pool) Env() PoolEnv { return p.env }

// Strategy is the configured selection strategy
func (p *Pool) Strategy() string { return p.strategy }

func (p *Pool) lockUnless(held bool) func() {
	if held {
		return func() {}
	}
	p.mu.Lock()
	return p.mu.Unlock
}

// HasCredentials reports whether any row exists
func (p *Pool) HasCredentials() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries) > 0
}

// EntryCount is the number of rows
func (p *Pool) EntryCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// HasAvailable reports at least one entry outside exhaustion cooldown for model
func (p *Pool) HasAvailable(model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	available, _ := p.availableEntries(false, false, model)
	return len(available) > 0
}

// LiftReopenedCooldowns clears elapsed or early reopened cooldowns then reports availability
func (p *Pool) LiftReopenedCooldowns(model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	available, _ := p.availableEntries(true, false, model)
	return len(available) > 0
}

// NextAvailableAt is the earliest time any entry re-enters rotation, false when available now or unknown
func (p *Pool) NextAvailableAt(model string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if available, _ := p.availableEntries(false, false, model); len(available) > 0 {
		return time.Time{}, false
	}
	sole := p.isSoleCredential()
	best, found := 0.0, false
	consider := func(until float64) {
		if !found || until < best {
			best, found = until, true
		}
	}
	for _, e := range p.entries {
		if e.LastStatus == StatusExhausted {
			if until, ok := exhaustedUntil(e, sole); ok {
				consider(until)
			}
		}
		if e.LastStatus != StatusDead {
			if until, ok := ModelCooldownUntil(e, model); ok {
				consider(until)
			}
		}
	}
	if !found {
		return time.Time{}, false
	}
	return epochTime(best), true
}

func epochTime(epoch float64) time.Time {
	sec := int64(epoch)
	return time.Unix(sec, int64((epoch-float64(sec))*1e9))
}

// Entries is a snapshot of the rows
func (p *Pool) Entries() []PooledCredential {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]PooledCredential(nil), p.entries...)
}

// DEAD entries never re-enter rotation, so one or fewer live rows means nothing to rotate to
func (p *Pool) isSoleCredential() bool {
	live := 0
	for _, e := range p.entries {
		if e.LastStatus != StatusDead {
			live++
		}
	}
	return live <= 1
}

func (p *Pool) findLocked(pred func(PooledCredential) bool) (PooledCredential, bool) {
	for _, e := range p.entries {
		if pred(e) {
			return e, true
		}
	}
	return PooledCredential{}, false
}

func (p *Pool) findByID(id string) (PooledCredential, bool) {
	return p.findLocked(func(e PooledCredential) bool { return e.ID == id })
}

func (p *Pool) currentLocked() (PooledCredential, bool) {
	if p.currentID == "" {
		return PooledCredential{}, false
	}
	return p.findByID(p.currentID)
}

// Current is the entry selected last, if it is still in the pool
func (p *Pool) Current() (PooledCredential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentLocked()
}

// CurrentID is the id of the entry selected last
func (p *Pool) CurrentID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentID
}

// EntryIDForAPIKey is the stable id of the runtime credential in use, the current selection when it still supplies the hint
func (p *Pool) EntryIDForAPIKey(apiKeyHint string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.currentLocked(); ok && (apiKeyHint == "" || cur.RuntimeAPIKey() == apiKeyHint) {
		return cur.ID
	}
	if apiKeyHint == "" {
		return ""
	}
	var matches []PooledCredential
	for _, e := range p.entries {
		if e.RuntimeAPIKey() == apiKeyHint {
			matches = append(matches, e)
		}
	}
	if len(matches) == 1 {
		return matches[0].ID
	}
	return ""
}

// ---- mutation primitives ----

// Swaps an entry in place by id preserving order
func (p *Pool) replaceEntry(updated PooledCredential, held bool) {
	defer p.lockUnless(held)()
	updated.scope = p.env.Scope
	for i, e := range p.entries {
		if e.ID == updated.ID {
			p.entries[i] = updated
			return
		}
	}
}

// Writes the rows to the owning store and adopts peer token generations the merge handed back
func (p *Pool) persist(held bool, removedIDs, statusClearedIDs []string) {
	defer p.lockUnless(held)()
	payloads := make([]map[string]any, 0, len(p.entries))
	for _, e := range p.entries {
		payloads = append(payloads, e.ToMap())
	}
	written, ok := PersistPoolEntries(p.env, p.provider, payloads, removedIDs, statusClearedIDs, p.persistedTokenPairs)
	if !ok {
		return
	}
	rows := entryIDs(written)
	p.persistedTokenPairs = TokenPairsByID(written)
	for _, e := range p.entries {
		row, present := rows[e.ID]
		pair, has := p.persistedTokenPairs[e.ID]
		if !present || !has || !pair.any() {
			continue
		}
		if anyString(pair.Access) != e.AccessToken || anyString(pair.Refresh) != e.RefreshToken {
			p.replaceEntry(PooledCredentialFromMap(p.provider, row), true)
		}
	}
}

// Swaps the updated copy into the pool, persists when asked and returns the live row
func (p *Pool) adopt(updated PooledCredential, held, persist bool) PooledCredential {
	defer p.lockUnless(held)()
	p.replaceEntry(updated, true)
	if persist {
		p.persist(true, nil, nil)
		if live, ok := p.findByID(updated.ID); ok {
			return live
		}
	}
	updated.scope = p.env.Scope
	return updated
}

// Drops every entry seeded from sources and persists the removal
func (p *Pool) quarantineSources(entry PooledCredential, sources map[string]bool, held bool) {
	defer p.lockUnless(held)()
	var removed []string
	kept := p.entries[:0:0]
	for _, e := range p.entries {
		if sources[e.Source] {
			removed = append(removed, e.ID)
			continue
		}
		kept = append(kept, e)
	}
	p.entries = kept
	if p.currentID == entry.ID {
		p.currentID = ""
	}
	p.persist(true, removed, nil)
}

// ---- exhaustion ----

// Upstream permanent OAuth failures that will not recover on TTL
func (p *Pool) isTerminalAuthFailure(statusCode int, normalized map[string]any) bool {
	reason := strings.ToLower(strings.TrimSpace(anyString(normalized["reason"])))
	if reason == CredentialPersistFailedReason {
		return true
	}
	return statusCode == 401 && terminalAuthReasons[reason]
}

func (p *Pool) markExhausted(entry PooledCredential, statusCode int, errorContext map[string]any, persist bool, failureReason string, held bool) PooledCredential {
	normalized := normalizeErrorContext(errorContext)
	terminal := entry.LastStatus == StatusDead || p.isTerminalAuthFailure(statusCode, normalized)
	u := entry.Clone()
	if failureReason != "" {
		u.Extra["failure_reason"] = failureReason
	} else {
		delete(u.Extra, "failure_reason")
	}
	u.LastStatus = StatusExhausted
	if terminal {
		u.LastStatus = StatusDead
	}
	u.LastStatusAt = nowEpoch()
	u.LastErrorCode = statusCode
	u.LastErrorReason = anyString(normalized["reason"])
	u.LastErrorMessage = anyString(normalized["message"])
	u.LastErrorResetAt, _ = anyFloat(normalized["reset_at"])
	return p.adopt(u, held, persist)
}

// ---- cross process token resync ----

// Syncs a claude_code entry from the Claude Code credentials file when its tokens differ
func (p *Pool) syncAnthropicEntryFromCredentialsFile(entry PooledCredential, held bool) PooledCredential {
	if p.provider != "anthropic" || entry.Source != "claude_code" {
		return entry
	}
	creds := ReadClaudeCodeCredentials(p.env)
	if creds == nil {
		return entry
	}
	fileRefresh, fileAccess := anyString(creds["refreshToken"]), anyString(creds["accessToken"])
	if (fileAccess != "" || fileRefresh != "") && ((fileAccess != "" && fileAccess != entry.AccessToken) || (fileRefresh != "" && fileRefresh != entry.RefreshToken)) {
		p.env.log().Debug(fmt.Sprintf("Pool entry %s: syncing tokens from credentials file (tokens changed)", entry.ID))
		u := entry.Clone().ClearStatus()
		if fileAccess != "" {
			u.AccessToken = fileAccess
		}
		if fileRefresh != "" {
			u.RefreshToken = fileRefresh
		}
		if ms, ok := anyFloat(creds["expiresAt"]); ok && ms != 0 {
			u.ExpiresAtMS = int64(ms)
		}
		return p.adopt(u, held, true)
	}
	return entry
}

// Adopts a token pair rotated by another pool instance for anthropic, xai and plugin providers
func (p *Pool) syncEntryFromPoolStore(entry PooledCredential, held bool) PooledCredential {
	if p.provider != "anthropic" && p.provider != "xai-oauth" && pluginRefreshHook(p.env.ctx(), p.provider) == nil {
		return entry
	}
	isAnthropic, isXAI := p.provider == "anthropic", p.provider == "xai-oauth"
	display := map[string]string{"anthropic": "Anthropic", "xai-oauth": "xAI"}[p.provider]
	if display == "" {
		display = p.provider
	}
	if isAnthropic && IsBorrowedCredentialSource(entry.Source, p.provider) {
		return entry
	}
	rows, err := p.env.Store.ReadCredentialPool(p.provider)
	if err != nil {
		p.env.log().Debug(fmt.Sprintf("Failed to sync %s OAuth entry from credential pool: %v", display, err))
		return entry
	}
	var persisted map[string]any
	for _, row := range rows {
		if anyString(row["id"]) == entry.ID {
			persisted = row
			break
		}
	}
	if persisted == nil {
		return entry
	}
	defer p.lockUnless(held)()
	p.persistedTokenPairs[entry.ID] = credentialTokenPair(persisted)
	stored := PooledCredentialFromMap(p.provider, persisted)
	if !isXAI && strings.TrimSpace(stored.AccessToken) == "" && strings.TrimSpace(stored.RefreshToken) == "" {
		return entry
	}
	if stored.AccessToken != entry.AccessToken || stored.RefreshToken != entry.RefreshToken {
		p.env.log().Debug(fmt.Sprintf("Pool entry %s: adopting %s OAuth tokens rotated by another pool instance", entry.ID, display))
		p.replaceEntry(stored, true)
		stored.scope = p.env.Scope
		return stored
	}
	return entry
}

// Syncs a Codex or xAI device_code entry from the auth.json tokens singleton
func (p *Pool) syncEntryFromAuthStore(entry PooledCredential, held bool) PooledCredential {
	spec, ok := tokensSingletonProviders[p.provider]
	if !ok {
		return entry
	}
	isCodex := p.provider == "openai-codex"
	if entry.Source != "device_code" && !(isCodex && entry.Source == SourceManualDeviceCode) {
		return entry
	}
	state, err := p.lockedProviderState(p.provider)
	if err != nil {
		p.env.log().Debug(fmt.Sprintf("Failed to sync %s entry from auth.json: %v", spec.display, err))
		return entry
	}
	tokens, ok := state["tokens"].(map[string]any)
	if !ok {
		return entry
	}
	if isCodex && !codexEntryTracksSingleton(entry, tokens) {
		return entry
	}
	storeAccess, storeRefresh := anyString(tokens["access_token"]), anyString(tokens["refresh_token"])
	shouldAdopt := storeAccess != "" && (storeAccess != entry.AccessToken || (storeRefresh != "" && storeRefresh != entry.RefreshToken))
	if !shouldAdopt && isCodex && storeRefresh != "" && storeRefresh != entry.RefreshToken && storeAccess == "" {
		p.env.log().Info(fmt.Sprintf("Pool entry %s: auth.json has newer refresh_token but no access_token; adopting refresh_token to avoid replaying consumed token", entry.ID))
		shouldAdopt = true
	}
	if shouldAdopt && singletonPredatesEntry(state, entry) {
		p.env.log().Info(fmt.Sprintf("Pool entry %s: auth.json singleton predates this entry's rotation (last_refresh %v < %s); keeping pool chain to avoid replaying the consumed refresh token", entry.ID, state["last_refresh"], entry.LastRefresh))
		shouldAdopt = false
	}
	if !shouldAdopt {
		return entry
	}
	p.env.log().Debug(fmt.Sprintf("Pool entry %s: syncing %s tokens from auth.json (refreshed by another process)", entry.ID, spec.display))
	u := entry.Clone().ClearStatus()
	if storeAccess != "" {
		u.AccessToken = storeAccess
	}
	if storeRefresh != "" {
		u.RefreshToken = storeRefresh
	}
	if lr := anyString(state["last_refresh"]); lr != "" {
		u.LastRefresh = lr
	}
	return p.adopt(u, held, true)
}

// Provider state read under the auth store lock
func (p *Pool) lockedProviderState(provider string) (map[string]any, error) {
	unlock, err := p.env.Store.Lock(AuthLockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := p.env.Store.Load()
	if err != nil {
		return nil, err
	}
	return p.env.Store.ProviderState(data, provider), nil
}

var nousComparableKeys = []string{"access_token", "refresh_token", "expires_at", "agent_key", "agent_key_expires_at", "inference_base_url"}

func nousEntryField(e PooledCredential, key string) string {
	switch key {
	case "access_token":
		return e.AccessToken
	case "refresh_token":
		return e.RefreshToken
	case "expires_at":
		return e.ExpiresAt
	case "agent_key":
		return e.AgentKey
	case "agent_key_expires_at":
		return e.AgentKeyExpiresAt
	case "inference_base_url":
		return e.InferenceBaseURL
	}
	return ""
}

func setNousEntryField(e *PooledCredential, key, value string) {
	switch key {
	case "access_token":
		e.AccessToken = value
	case "refresh_token":
		e.RefreshToken = value
	case "expires_at":
		e.ExpiresAt = value
	case "agent_key":
		e.AgentKey = value
	case "agent_key_expires_at":
		e.AgentKeyExpiresAt = value
	case "inference_base_url":
		e.InferenceBaseURL = value
	}
}

// Syncs a Nous device_code entry from auth.json providers.nous when the state differs
func (p *Pool) syncNousEntryFromAuthStore(entry PooledCredential, held bool) PooledCredential {
	if p.provider != "nous" || entry.Source != "device_code" {
		return entry
	}
	state, err := p.lockedProviderState("nous")
	if err != nil {
		p.env.log().Debug(fmt.Sprintf("Failed to sync Nous entry from auth.json: %v", err))
		return entry
	}
	if len(state) == 0 {
		return entry
	}
	differs := false
	for _, key := range nousComparableKeys {
		if v := anyString(state[key]); v != "" && nousEntryField(entry, key) != v {
			differs = true
			break
		}
	}
	if !differs {
		return entry
	}
	p.env.log().Debug(fmt.Sprintf("Pool entry %s: syncing Nous state from auth.json", entry.ID))
	u := entry.Clone().ClearStatus()
	for _, key := range nousComparableKeys {
		if v := anyString(state[key]); v != "" {
			setNousEntryField(&u, key, v)
		}
	}
	for _, key := range nousExtraStateKeys {
		if v, ok := state[key]; ok && v != nil {
			u.Extra[key] = v
		}
	}
	return p.adopt(u, held, true)
}

// Writes refreshed device_code tokens back to the provider's auth.json state, root only when the grant came from root
func (p *Pool) syncDeviceCodeEntryToAuthStore(entry PooledCredential) {
	_, tokensProvider := tokensSingletonProviders[p.provider]
	if entry.Source != "device_code" || (p.provider != "nous" && !tokensProvider) {
		return
	}
	err := func() error {
		unlock, err := p.env.Store.Lock(AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		data, err := p.env.Store.Load()
		if err != nil {
			return err
		}
		state, sourcePath := p.env.Store.ProviderStateWithSource(data, p.provider)
		if state == nil {
			return nil
		}
		globalRoot := p.env.Store.globalPath
		fromRoot := sourcePath != "" && globalRoot != "" && samePath(sourcePath, globalRoot)
		if !p.applyEntryToSingletonState(entry, state) {
			return nil
		}
		if fromRoot {
			writeThroughProviderStateToGlobalRoot(p.env, p.provider, state)
			return nil
		}
		if err := p.env.Store.StoreProviderState(data, p.provider, state, false); err != nil {
			return err
		}
		return p.env.Store.Save(data)
	}()
	if err != nil {
		p.env.log().Debug(fmt.Sprintf("Failed to sync %s pool entry back to auth store: %v", p.provider, err))
	}
}

// Copies the entry tokens into the provider's auth.json state in place
func (p *Pool) applyEntryToSingletonState(entry PooledCredential, state map[string]any) bool {
	if p.provider == "nous" {
		state["access_token"] = entry.AccessToken
		for _, key := range []string{"refresh_token", "expires_at", "agent_key", "agent_key_expires_at"} {
			if v := nousEntryField(entry, key); v != "" {
				state[key] = v
			}
		}
		for _, key := range nousExtraStateKeys {
			if v, ok := entry.Extra[key]; ok && v != nil {
				state[key] = v
			}
		}
		if entry.InferenceBaseURL != "" {
			state["inference_base_url"] = entry.InferenceBaseURL
		}
		return true
	}
	tokens, ok := state["tokens"].(map[string]any)
	if !ok {
		return false
	}
	tokens["access_token"] = entry.AccessToken
	if entry.RefreshToken != "" {
		tokens["refresh_token"] = entry.RefreshToken
	}
	if entry.LastRefresh != "" {
		state["last_refresh"] = entry.LastRefresh
	}
	return true
}

// ---- refresh ----

type refreshDone struct {
	result PooledCredential
	ok     bool
}

func (r refreshDone) Error() string { return "refresh short circuit" }

// Refreshes one OAuth entry, serializing single use providers under the cross process auth store lock
func (p *Pool) refreshEntry(entry PooledCredential, force bool, failedKey string, held bool) (PooledCredential, bool) {
	if entry.AuthType != AuthTypeOAuth || entry.RefreshToken == "" {
		if force {
			p.markExhausted(entry, 0, nil, true, "", held)
		}
		return PooledCredential{}, false
	}
	if !singleUseRefreshProviders[p.provider] && pluginRefreshHook(p.env.ctx(), p.provider) == nil {
		return p.refreshEntryImpl(entry, force, failedKey, held)
	}
	unlock, err := p.env.Store.Lock(p.singleUseRefreshLockTimeout())
	if err != nil {
		p.env.log().Debug(fmt.Sprintf("Credential refresh failed for %s/%s: %v", p.provider, entry.ID, err))
		return p.recoverFailedRefresh(entry, err, held)
	}
	defer unlock()
	if p.provider == "openai-codex" {
		synced := p.syncEntryFromAuthStore(entry, held)
		if !sameTokens(synced, entry) && !force && !p.entryNeedsRefresh(synced) {
			return synced, true
		}
		return p.refreshEntryImpl(synced, force, "", held)
	}
	synced := p.syncEntryFromPoolStore(entry, held)
	if p.provider == "anthropic" && synced.Source == "claude_code" {
		unlockFile, err := p.env.Store.fileLock(ClaudeCodeCredentialsPath(), p.singleUseRefreshLockTimeout())
		if err != nil {
			p.env.log().Debug(fmt.Sprintf("Credential refresh failed for %s/%s: %v", p.provider, entry.ID, err))
			return p.recoverFailedRefresh(synced, err, held)
		}
		defer unlockFile()
		synced = p.syncAnthropicEntryFromCredentialsFile(synced, held)
		if synced.RefreshToken != entry.RefreshToken {
			return synced, true
		}
		return p.refreshEntryImpl(synced, force, "", held)
	}
	if synced.AccessToken != entry.AccessToken || synced.RefreshToken != entry.RefreshToken {
		return synced, true
	}
	return p.refreshEntryImpl(synced, force, "", held)
}

func sameTokens(a, b PooledCredential) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken && a.LastStatus == b.LastStatus && a.LastRefresh == b.LastRefresh
}

// Quarantines an entry whose rotated pair never reached its store
func (p *Pool) failClosedUnpersistedRotation(entry PooledCredential, cause error, store string, held bool) {
	p.env.log().Error(fmt.Sprintf("Anthropic %s refresh rotated the single-use token but could not commit it to %s (%v) — failing closed and quarantining the credential; re-authenticate to recover", entry.Source, store, cause))
	MarkRotationConsumedUncommitted(SpentRotationSourcePath(p.env, entry.Source), entry.AccessToken, entry.RefreshToken)
	p.markExhausted(entry, 0, map[string]any{"reason": CredentialPersistFailedReason,
		"message": fmt.Sprintf("rotated credential was not durably written to %s: %v", store, cause)}, true, "", held)
}

// Configured refresh POST timeout plus margin so a slow token endpoint cannot starve the flock
func (p *Pool) singleUseRefreshLockTimeout() time.Duration {
	envVar, ok := refreshTimeoutEnvVars[p.provider]
	if !ok {
		envVar = "ICEBATH_ANTHROPIC_REFRESH_TIMEOUT_SECONDS"
	}
	refresh := envFloat(envVar, 20)
	candidate := time.Duration((refresh + 5.0) * float64(time.Second))
	if candidate < AuthLockTimeout {
		return AuthLockTimeout
	}
	return candidate
}

func (p *Pool) refreshTimeout() time.Duration {
	envVar, ok := refreshTimeoutEnvVars[p.provider]
	if !ok {
		envVar = "ICEBATH_ANTHROPIC_REFRESH_TIMEOUT_SECONDS"
	}
	return time.Duration(envFloat(envVar, 20) * float64(time.Second))
}

// Writes a rotated Anthropic pair to its authoritative singleton or fails closed
func (p *Pool) commitAnthropicRotation(entry PooledCredential, refreshed map[string]any, held bool) error {
	var store string
	switch entry.Source {
	case "claude_code":
		store = "~/.claude/.credentials.json"
	case "hermes_pkce":
		store = "~/.icebath/.anthropic_oauth.json"
	default:
		return nil
	}
	access, refresh := anyString(refreshed["access_token"]), anyString(refreshed["refresh_token"])
	ms, _ := anyFloat(refreshed["expires_at_ms"])
	var err error
	if entry.Source == "claude_code" {
		err = WriteClaudeCodeCredentials(p.env, access, refresh, int64(ms), nil, entry.RefreshToken)
	} else {
		err = WriteHermesOAuthCredentials(p.env, access, refresh, int64(ms), singletonTargetForEntry(p, entry))
	}
	if err != nil {
		p.failClosedUnpersistedRotation(entry, err, store, held)
		return refreshDone{}
	}
	return nil
}

// Root .anthropic_oauth.json when the entry is a borrowed hermes_pkce row, else empty
func singletonTargetForEntry(p *Pool, entry PooledCredential) string {
	if entry.Source != "hermes_pkce" || !p.borrowedRootIDs[entry.ID] {
		return ""
	}
	return RootHermesOAuthFile(p.env)
}

// POSTs the Anthropic refresh, commits to the singleton and returns the rotated unpersisted entry
func (p *Pool) refreshAnthropic(entry PooledCredential, held bool) (PooledCredential, error) {
	sourcePath := SpentRotationSourcePath(p.env, entry.Source)
	if IsRotationConsumedUncommitted(entry.RefreshToken, sourcePath) || IsRotationConsumedUncommitted(entry.AccessToken, sourcePath) {
		store := sourcePath
		if store == "" {
			store = "credential store"
		}
		p.failClosedUnpersistedRotation(entry, errors.New("credential pair was rotated by another process but the rotation never committed (spent-rotation sidecar verdict)"), store, held)
		return PooledCredential{}, refreshDone{}
	}
	refreshed, err := RefreshAnthropicOAuthPure(p.env, entry.RefreshToken, strings.HasSuffix(entry.Source, "hermes_pkce"))
	if err != nil {
		return PooledCredential{}, err
	}
	u := entry.Clone()
	u.AccessToken = anyString(refreshed["access_token"])
	u.RefreshToken = anyString(refreshed["refresh_token"])
	ms, _ := anyFloat(refreshed["expires_at_ms"])
	u.ExpiresAtMS = int64(ms)
	if err := p.commitAnthropicRotation(entry, refreshed, held); err != nil {
		return PooledCredential{}, err
	}
	return u, nil
}

// Codex and xAI refresh POST returning the rotated unpersisted entry
func (p *Pool) postTokensRefresh(entry PooledCredential) (PooledCredential, error) {
	spec := tokensSingletonProviders[p.provider]
	refreshed, err := spec.refresh(p.env.ctx(), entry.AccessToken, entry.RefreshToken, p.refreshTimeout())
	if err != nil {
		return PooledCredential{}, err
	}
	u := entry.Clone()
	u.AccessToken = anyString(refreshed["access_token"])
	u.RefreshToken = anyString(refreshed["refresh_token"])
	u.LastRefresh = anyString(refreshed["last_refresh"])
	return u, nil
}

func (p *Pool) refreshEntryImpl(entry PooledCredential, force bool, failedKey string, held bool) (PooledCredential, bool) {
	var updated PooledCredential
	var err error
	switch {
	case p.provider == "anthropic":
		updated, err = p.refreshAnthropic(entry, held)
	case tokensSingletonProviders[p.provider].refresh != nil:
		entry = p.syncEntryFromAuthStore(entry, held)
		updated, err = p.postTokensRefresh(entry)
	case pluginRefreshHook(p.env.ctx(), p.provider) != nil:
		var rotated map[string]any
		rotated, err = pluginRefreshHook(p.env.ctx(), p.provider)(entry)
		if err == nil && len(rotated) == 0 {
			err = errors.New("provider refresh_credential returned no rotated fields")
		}
		if err == nil {
			updated = ApplyPluginRefreshResult(entry, rotated)
		}
	case p.provider == "nous":
		staleKey := failedKey
		if staleKey == "" {
			staleKey = entry.RuntimeAPIKey()
		}
		if staleKey == "" {
			staleKey = entry.AgentKey
		}
		if staleKey == "" {
			staleKey = entry.AccessToken
		}
		synced := p.syncNousEntryFromAuthStore(entry, held)
		if !sameNousState(synced, entry) {
			entry = synced
			if force && entry.RuntimeAPIKey() != "" && entry.RuntimeAPIKey() != staleKey {
				p.env.log().Debug(fmt.Sprintf("Nous entry %s: adopting peer-rotated token, skipping refresh", entry.ID))
				return entry, true
			}
		}
		_, err = nousRuntimeCredentialsFunc(p.env, force, staleKey)
		if err == nil {
			updated = p.syncNousEntryFromAuthStore(entry, held)
		}
	default:
		return entry, true
	}
	if err != nil {
		var done refreshDone
		if errors.As(err, &done) {
			return done.result, done.ok
		}
		p.env.log().Debug(fmt.Sprintf("Credential refresh failed for %s/%s: %v", p.provider, entry.ID, err))
		return p.recoverFailedRefresh(entry, err, held)
	}
	updated = updated.MarkOK()
	p.replaceEntry(updated, held)
	p.persist(held, nil, []string{updated.ID})
	p.syncDeviceCodeEntryToAuthStore(updated)
	return updated, true
}

func sameNousState(a, b PooledCredential) bool {
	for _, key := range nousComparableKeys {
		if nousEntryField(a, key) != nousEntryField(b, key) {
			return false
		}
	}
	return a.LastStatus == b.LastStatus
}

// After a failed refresh POST adopt a peer rotation, quarantine a dead grant, or bench the row
func (p *Pool) recoverFailedRefresh(entry PooledCredential, cause error, held bool) (PooledCredential, bool) {
	log := p.env.log()
	switch {
	case p.provider == "anthropic":
		if entry.Source == "claude_code" {
			synced := p.syncAnthropicEntryFromCredentialsFile(entry, held)
			if synced.RefreshToken != entry.RefreshToken {
				log.Debug("Retrying refresh with synced token from credentials file")
				refreshed, retryErr := RefreshAnthropicOAuthPure(p.env, synced.RefreshToken, strings.HasSuffix(synced.Source, "hermes_pkce"))
				if retryErr == nil {
					if commitErr := p.commitAnthropicRotation(synced, refreshed, held); commitErr != nil {
						var done refreshDone
						if errors.As(commitErr, &done) {
							return done.result, done.ok
						}
					} else {
						u := synced.Clone()
						u.AccessToken = anyString(refreshed["access_token"])
						u.RefreshToken = anyString(refreshed["refresh_token"])
						ms, _ := anyFloat(refreshed["expires_at_ms"])
						u.ExpiresAtMS = int64(ms)
						u.LastStatus, u.LastStatusAt, u.LastErrorCode = StatusOK, 0, 0
						return p.adopt(u, held, true), true
					}
				} else {
					log.Debug(fmt.Sprintf("Retry refresh also failed: %v", retryErr))
				}
			} else if !p.entryNeedsRefresh(synced) {
				log.Debug("Credentials file has valid token, using without refresh")
				return synced, true
			}
		} else {
			synced := p.syncEntryFromPoolStore(entry, held)
			if synced.RefreshToken != entry.RefreshToken {
				log.Debug("Anthropic OAuth refresh failed but pool store has newer tokens — adopting")
				return p.adopt(synced.MarkOK(), held, true), true
			}
		}
		if IsTerminalAnthropicRefreshError(cause) {
			log.Warn(fmt.Sprintf("Anthropic OAuth refresh token for %s is terminally invalid (%v); the credential leaves rotation. Re-run 'icebath auth add anthropic' to sign in again.", entryLabel(entry), cause))
			p.markDeadRefreshGrant(entry, cause, held)
			return PooledCredential{}, false
		}
	case tokensSingletonProviders[p.provider].refresh != nil:
		spec := tokensSingletonProviders[p.provider]
		synced := p.syncEntryFromAuthStore(entry, held)
		if synced.RefreshToken != entry.RefreshToken {
			log.Debug(fmt.Sprintf("%s OAuth refresh failed but auth.json has newer tokens — adopting", spec.short))
			return p.adopt(synced.MarkOK(), held, true), true
		}
		if spec.terminal(cause) {
			log.Warn(fmt.Sprintf("%s OAuth refresh token is terminally invalid (%v); clearing local token state. Re-run 'icebath auth add %s' to sign in again.", spec.short, cause, p.provider))
			p.clearTerminalTokensState(entry, cause)
			p.quarantineSources(entry, map[string]bool{"device_code": true}, held)
			p.markDeadRefreshGrant(entry, cause, held)
			return PooledCredential{}, false
		}
	case p.provider == "nous":
		synced := p.syncNousEntryFromAuthStore(entry, held)
		if synced.RefreshToken != entry.RefreshToken {
			log.Debug("Nous refresh failed but auth.json has newer tokens — adopting")
			updated := p.adopt(synced.MarkOK(), held, true)
			p.syncDeviceCodeEntryToAuthStore(updated)
			return updated, true
		}
		var lockErr *LockTimeoutError
		if errors.As(cause, &lockErr) {
			log.Debug("Nous refresh skipped: auth store lock busy; not benching entry")
			return entry, true
		}
		if isTerminalNousRefreshError(cause) {
			log.Warn(fmt.Sprintf("Nous refresh token is terminally invalid (%v); clearing local token state. Re-run 'icebath auth add nous' to sign in again.", cause))
			p.clearTerminalNousState(entry, cause)
			p.quarantineSources(entry, map[string]bool{NousDeviceCodeSource: true, "manual:" + NousDeviceCodeSource: true}, held)
			p.markDeadRefreshGrant(entry, cause, held)
			return PooledCredential{}, false
		}
	case pluginRefreshHook(p.env.ctx(), p.provider) != nil:
		if result, ok, handled := RecoverFailedPluginRefresh(p, entry, cause, held); handled {
			return result, ok
		}
	}
	p.markExhausted(entry, 0, nil, true, "", held)
	return PooledCredential{}, false
}

func entryLabel(e PooledCredential) string {
	if e.Label != "" {
		return e.Label
	}
	if len(e.ID) > 8 {
		return e.ID[:8]
	}
	return e.ID
}

func authErrorCode(err error) string {
	var ae *AuthError
	if errors.As(err, &ae) && ae.Code != "" {
		return ae.Code
	}
	return ""
}

// Marks a row whose refresh token was terminally rejected DEAD when the quarantine kept it
func (p *Pool) markDeadRefreshGrant(entry PooledCredential, cause error, held bool) {
	defer p.lockUnless(held)()
	current, ok := p.findByID(entry.ID)
	if !ok || current.LastStatus == StatusDead {
		return
	}
	u := current.Clone()
	u.LastStatus, u.LastStatusAt, u.LastErrorCode = StatusDead, nowEpoch(), 0
	u.LastErrorReason = authErrorCode(cause)
	if u.LastErrorReason == "" {
		u.LastErrorReason = "invalid_grant"
	}
	u.LastErrorMessage = cause.Error()
	p.adopt(u, true, true)
}

// Drops the dead Codex or xAI token pair from auth.json unless a peer already rotated it
func (p *Pool) clearTerminalTokensState(entry PooledCredential, cause error) {
	spec := tokensSingletonProviders[p.provider]
	err := func() error {
		unlock, err := p.env.Store.Lock(AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		data, err := p.env.Store.Load()
		if err != nil {
			return err
		}
		state := p.env.Store.ProviderState(data, p.provider)
		if state == nil {
			state = map[string]any{}
		}
		tokens, ok := state["tokens"].(map[string]any)
		if !ok {
			return nil
		}
		storeRefresh := strings.TrimSpace(anyString(tokens["refresh_token"]))
		if storeRefresh != "" && storeRefresh != strings.TrimSpace(entry.RefreshToken) {
			return nil
		}
		delete(tokens, "access_token")
		delete(tokens, "refresh_token")
		state["tokens"] = tokens
		code := authErrorCode(cause)
		if code == "" {
			code = "unknown"
		}
		state["last_auth_error"] = map[string]any{"provider": p.provider, "code": code, "message": cause.Error(),
			"reason": "credential_pool_refresh_failure", "relogin_required": true, "at": utcNowISO()}
		if err := p.env.Store.StoreProviderState(data, p.provider, state, false); err != nil {
			return err
		}
		return p.env.Store.Save(data)
	}()
	if err != nil {
		p.env.log().Debug(fmt.Sprintf("Failed to clear terminal %s OAuth state: %v", spec.short, err))
	}
}

func (p *Pool) clearTerminalNousState(entry PooledCredential, cause error) {
	err := func() error {
		unlock, err := p.env.Store.Lock(AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		data, err := p.env.Store.Load()
		if err != nil {
			return err
		}
		state := p.env.Store.ProviderState(data, "nous")
		if state == nil {
			state = map[string]any{"client_id": entry.Extra["client_id"], "portal_base_url": entry.Extra["portal_base_url"],
				"inference_base_url": nilIfEmpty(entry.InferenceBaseURL), "token_type": entry.Extra["token_type"],
				"scope": entry.Extra["scope"], "tls": entry.Extra["tls"]}
		}
		storeRefresh := strings.TrimSpace(anyString(state["refresh_token"]))
		if storeRefresh != "" && storeRefresh != strings.TrimSpace(entry.RefreshToken) {
			return nil
		}
		quarantineNousOAuthState(state, cause, "credential_pool_refresh_failure")
		quarantineNousPoolEntries(data, cause, "credential_pool_refresh_failure")
		if err := p.env.Store.StoreProviderState(data, "nous", state, false); err != nil {
			return err
		}
		return p.env.Store.Save(data)
	}()
	if err != nil {
		p.env.log().Debug(fmt.Sprintf("Failed to clear terminal Nous OAuth state: %v", err))
	}
}

// True when persisted pool entry error metadata describes a 429 or quota stop
func isCodexRateLimitShaped(code int, reason, message string) bool {
	reasonL, messageL := strings.ToLower(reason), strings.ToLower(message)
	if code == 429 {
		return true
	}
	for _, k := range []string{"rate_limit", "usage_limit", "quota"} {
		if strings.Contains(reasonL, k) {
			return true
		}
	}
	for _, k := range []string{"rate limit", "usage limit", "quota"} {
		if strings.Contains(messageL, k) {
			return true
		}
	}
	return false
}

// Live check whether an exhausted Codex entry's quota reset early, throttled per token
func (p *Pool) codexQuotaRestoredUpstream(entry PooledCredential, held bool) bool {
	if p.provider != "openai-codex" || entry.LastStatus != StatusExhausted {
		return false
	}
	if !isCodexRateLimitShaped(entry.LastErrorCode, entry.LastErrorReason, entry.LastErrorMessage) {
		return false
	}
	token := entry.AccessToken
	if token == "" {
		return false
	}
	if fresh, ok := refreshExpiredCodexProbeToken(p.env, token, entry.RefreshToken); ok {
		u := entry.Clone()
		u.AccessToken = anyString(fresh["access_token"])
		u.RefreshToken = anyString(fresh["refresh_token"])
		if lr := anyString(fresh["last_refresh"]); lr != "" {
			u.LastRefresh = lr
		}
		entry = p.adopt(u, held, true)
		p.syncDeviceCodeEntryToAuthStore(entry)
		if entry.AccessToken != "" {
			token = entry.AccessToken
		}
	}
	restored, known := probeCodexQuotaRestoredFn(p.env, token, codexPoolRouteBaseURL(p.env, entry.BaseURL))
	return known && restored
}

func (p *Pool) entryNeedsRefresh(entry PooledCredential) bool {
	if entry.AuthType != AuthTypeOAuth {
		return false
	}
	if p.provider == "anthropic" || pluginRefreshHook(p.env.ctx(), p.provider) != nil {
		return PluginRowIsExpiring(entry)
	}
	switch p.provider {
	case "openai-codex":
		return codexAccessTokenIsExpiring(entry.AccessToken, CodexAccessTokenRefreshSkew)
	case "xai-oauth":
		return xaiAccessTokenIsExpiring(entry.AccessToken, xaiProactiveRefreshSkew(entry.AccessToken))
	}
	return false
}

// Whether a Codex JWT expires within the skew
func codexAccessTokenIsExpiring(accessToken string, skew time.Duration) bool {
	exp, ok := anyFloat(DecodeJWTClaims(accessToken)["exp"])
	if skew < 0 {
		skew = 0
	}
	return ok && exp <= nowEpoch()+skew.Seconds()
}

func xaiJWTExp(accessToken string) (float64, bool) {
	return anyFloat(DecodeJWTClaims(accessToken)["exp"])
}

func xaiAccessTokenIsExpiring(accessToken string, skew time.Duration) bool {
	exp, ok := xaiJWTExp(accessToken)
	if skew < 0 {
		skew = 0
	}
	return ok && exp <= nowEpoch()+skew.Seconds()
}

// Proactive refresh lead time before a short lived xAI JWT expires
func xaiProactiveRefreshSkew(accessToken string) time.Duration {
	maxSkew := XAIAccessTokenRefreshSkew
	exp, ok := xaiJWTExp(accessToken)
	if !ok {
		return maxSkew
	}
	remaining := exp - nowEpoch()
	if remaining > 0 && remaining <= 45*60 {
		return minDuration(120*time.Second, maxSkew)
	}
	return maxSkew
}
