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
var SingleUseRefreshPoolProviders = map[string]bool{"anthropic": true, "openai-codex": true, "xai-oauth": true}

// Refresh of one provider's OAuth login rows and the failures that end its grant
type oauthRefreshSpec struct {
	short    string
	refresh  func(ctx context.Context, entry PooledCredential, timeout time.Duration) (map[string]any, error)
	terminal func(err error) bool
}

// Providers whose OAuth login rows the pool refreshes through a token POST of their own
var oauthRefreshProviders = map[string]oauthRefreshSpec{
	"openai-codex": {short: "Codex", terminal: isTerminalCodexOAuthRefreshError,
		refresh: func(ctx context.Context, e PooledCredential, timeout time.Duration) (map[string]any, error) {
			return refreshCodexOAuthPure(ctx, e.AccessToken, e.RefreshToken, timeout)
		}},
	"xai-oauth": {short: "xAI", terminal: isTerminalXAIOAuthRefreshError,
		refresh: func(ctx context.Context, e PooledCredential, timeout time.Duration) (map[string]any, error) {
			return refreshXAIOAuthPure(ctx, e.AccessToken, e.RefreshToken, timeout)
		}},
	"minimax-oauth": {short: "MiniMax", terminal: isTerminalMinimaxOAuthRefreshError, refresh: refreshMinimaxLogin},
}

// RefreshableOAuthProviders are the built in providers whose pooled OAuth rows the pool can refresh
var RefreshableOAuthProviders = map[string]bool{"anthropic": true, "openai-codex": true, "xai-oauth": true, "minimax-oauth": true}

// Lock free window between consecutive auth store holds in a deferred refresh sweep
const refreshSweepSpacing = 500 * time.Millisecond

var refreshTimeoutEnvVars = map[string]string{"openai-codex": "ICEBATH_CODEX_REFRESH_TIMEOUT_SECONDS", "xai-oauth": "ICEBATH_XAI_REFRESH_TIMEOUT_SECONDS"}

// Linked source whose exhausted or DEAD row a re-auth in the tool that owns it may revive
var resyncSource = map[string]string{"anthropic": "claude_code"}

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

// Logger of the pool environment or the process default, named after this package
func (env PoolEnv) log() *slog.Logger {
	if env.Log != nil {
		return env.Log.With("logger", logName)
	}
	return pkgLog()
}

func (env PoolEnv) ctx() context.Context {
	if env.Ctx != nil {
		return env.Ctx
	}
	return context.Background()
}

// Call context profile hooks read through, the env's home and explicit secret scope bound onto it
func (env PoolEnv) hookContext() context.Context {
	h := ""
	if env.Store != nil {
		h = env.Store.Home()
	}
	ctx := ctxForHome(env.ctx(), h)
	if env.Scope != nil && env.Scope.Mapping != nil && boundScope(ctx) == nil {
		ctx = WithSecretScope(ctx, env.Scope.Mapping, env.Scope.ProfileHome)
	}
	return ctx
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
	// Login selection a view was narrowed to, nil for a whole pool
	choice *LoginChoice
	// Logins an exhausted pinned view falls over to under on_exhausted rotate
	spare []PooledCredential
}

// NewPool builds a pool over entries sorted by priority
func NewPool(env PoolEnv, provider string, entries []PooledCredential) *Pool {
	sorted := append([]PooledCredential(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })
	for i := range sorted {
		sorted[i].scope = env.Scope
	}
	return &Pool{provider: provider, env: env, entries: sorted, borrowedRootIDs: map[string]bool{},
		persistedTokenPairs: map[string]TokenPair{}, strategy: GetPoolStrategy(env.loginCtx(), provider),
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
	p.dropUnwritten(rows)
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

// Whether the pool refreshes the provider's OAuth rows through a token request of its own
func (p *Pool) refreshes() bool {
	return p.provider == "anthropic" || oauthRefreshProviders[p.provider].refresh != nil || pluginRefreshHook(p.env.ctx(), p.provider) != nil
}

// Adopts a token pair another pool instance rotated and stored for the row
func (p *Pool) syncEntryFromPoolStore(entry PooledCredential, held bool) PooledCredential {
	if !p.refreshes() {
		return entry
	}
	isAnthropic, isXAI := p.provider == "anthropic", p.provider == "xai-oauth"
	display := oauthRefreshProviders[p.provider].short
	if isAnthropic {
		display = "Anthropic"
	}
	if display == "" {
		display = p.provider
	}
	if isAnthropic && IsBorrowedCredentialSource(entry.Source) {
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

// ---- refresh ----

type refreshDone struct {
	result PooledCredential
	ok     bool
}

func (r refreshDone) Error() string { return "refresh short circuit" }

// Refreshes one OAuth entry under the cross process auth store lock, adopting a pair another instance rotated first
func (p *Pool) refreshEntry(entry PooledCredential, force bool, failedKey string, held bool) (PooledCredential, bool) {
	if entry.AuthType != AuthTypeOAuth || entry.RefreshToken == "" {
		if force {
			p.markExhausted(entry, 0, nil, true, "", held)
		}
		return PooledCredential{}, false
	}
	if !p.refreshes() {
		return p.refreshEntryImpl(entry, force, failedKey, held)
	}
	unlock, err := p.env.Store.Lock(p.singleUseRefreshLockTimeout())
	if err != nil {
		p.env.log().Debug(fmt.Sprintf("Credential refresh failed for %s/%s: %v", p.provider, entry.ID, err))
		return p.recoverFailedRefresh(entry, err, held)
	}
	defer unlock()
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

// Writes a rotated pair of the linked Claude Code login back to its credentials file or fails closed
func (p *Pool) commitAnthropicRotation(entry PooledCredential, refreshed map[string]any, held bool) error {
	if entry.Source != "claude_code" {
		return nil
	}
	ms, _ := anyFloat(refreshed["expires_at_ms"])
	if err := WriteClaudeCodeCredentials(p.env, anyString(refreshed["access_token"]), anyString(refreshed["refresh_token"]), int64(ms), nil, entry.RefreshToken); err != nil {
		p.failClosedUnpersistedRotation(entry, err, "~/.claude/.credentials.json", held)
		return refreshDone{}
	}
	return nil
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
	refreshed, err := RefreshAnthropicOAuthPure(p.env, entry.RefreshToken, strings.HasSuffix(entry.Source, IcebathPKCESource))
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

// Token POST of an OAuth login row returning the rotated unpersisted entry
func (p *Pool) postTokensRefresh(entry PooledCredential) (PooledCredential, error) {
	refreshed, err := oauthRefreshProviders[p.provider].refresh(p.env.ctx(), entry, p.refreshTimeout())
	if err != nil {
		return PooledCredential{}, err
	}
	u := entry.Clone()
	u.AccessToken = anyString(refreshed["access_token"])
	u.RefreshToken = anyString(refreshed["refresh_token"])
	u.LastRefresh = anyString(refreshed["last_refresh"])
	if ms, ok := anyFloat(refreshed["expires_at_ms"]); ok {
		u.ExpiresAtMS = int64(ms)
	}
	return u, nil
}

func (p *Pool) refreshEntryImpl(entry PooledCredential, force bool, failedKey string, held bool) (PooledCredential, bool) {
	var updated PooledCredential
	var err error
	switch {
	case p.provider == "anthropic":
		updated, err = p.refreshAnthropic(entry, held)
	case oauthRefreshProviders[p.provider].refresh != nil:
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
	return updated, true
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
				refreshed, retryErr := RefreshAnthropicOAuthPure(p.env, synced.RefreshToken, strings.HasSuffix(synced.Source, IcebathPKCESource))
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
	case oauthRefreshProviders[p.provider].refresh != nil:
		spec := oauthRefreshProviders[p.provider]
		synced := p.syncEntryFromPoolStore(entry, held)
		if synced.RefreshToken != entry.RefreshToken {
			log.Debug(fmt.Sprintf("%s OAuth refresh failed but auth.json has newer tokens for the login — adopting", spec.short))
			return p.adopt(synced.MarkOK(), held, true), true
		}
		if spec.terminal(cause) {
			log.Warn(fmt.Sprintf("%s OAuth refresh token of %s is terminally invalid (%v); the login leaves rotation. Re-run 'icebath auth add %s' to sign in again.", spec.short, entryLabel(entry), cause, p.provider))
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
	case "minimax-oauth":
		return entry.ExpiresAtMS != 0 && entry.ExpiresAtMS <= nowFunc().Add(MinimaxOAuthRefreshSkew).UnixMilli()
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
