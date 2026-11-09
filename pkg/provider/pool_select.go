package provider

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// Epoch of an icebath auth reset persisted by another process after the entry's status, false when none
func (p *Pool) resetClearedAfter(entry PooledCredential) (float64, bool) {
	rows, err := p.env.Store.ReadCredentialPool(p.provider)
	if err != nil {
		p.env.log().Debug(fmt.Sprintf("Pool entry %s: could not read reset marker: %v", entry.ID, err))
		return 0, false
	}
	for _, row := range rows {
		if anyString(row["id"]) != entry.ID {
			continue
		}
		cleared, ok := ParseAbsoluteTimestamp(row["status_cleared_at"])
		if ok && cleared != 0 && cleared > entry.LastStatusAt {
			return cleared, true
		}
		return 0, false
	}
	return 0, false
}

// Re-reads an exhausted or DEAD singleton seeded entry from its token authority
func (p *Pool) resyncStaleEntry(entry PooledCredential) PooledCredential {
	if entry.LastStatus != StatusExhausted && entry.LastStatus != StatusDead {
		return entry
	}
	if clearedAt, ok := p.resetClearedAfter(entry); ok {
		u := entry.Clone().MarkOK()
		u.StatusClearedAt = clearedAt
		return p.adopt(u, true, false)
	}
	if entry.Source != resyncSource[p.provider] {
		return entry
	}
	switch p.provider {
	case "anthropic":
		return p.syncAnthropicEntryFromCredentialsFile(entry, true)
	case "nous":
		return p.syncNousEntryFromAuthStore(entry, true)
	}
	return p.syncEntryFromAuthStore(entry, true)
}

// Entries outside cooldown plus the single use rows whose refresh is deferred past the lock, caller holds the lock
func (p *Pool) availableEntries(clearExpired, refresh bool, model string) ([]PooledCredential, []PooledCredential) {
	now := nowEpoch()
	clearedAny := false
	var prune []string
	var available, pending []PooledCredential
	sole := p.isSoleCredential()
	for _, original := range append([]PooledCredential(nil), p.entries...) {
		entry := original
		if entry.AuthType == AuthTypeAPIKey && entry.RuntimeAPIKey() == "" {
			continue
		}
		synced := p.resyncStaleEntry(entry)
		if !sameRow(synced, entry) {
			entry = synced
			clearedAny = true
		}
		if entry.LastStatus == StatusDead {
			if isManualSource(entry.Source) && entry.LastStatusAt != 0 && now-entry.LastStatusAt > DeadManualPruneTTL.Seconds() {
				reason := entry.LastErrorReason
				if reason == "" {
					reason = "unknown"
				}
				p.env.log().Warn(fmt.Sprintf("credential pool: pruning DEAD manual entry %s (reason=%s, age=%.1fh) — re-add via `icebath auth add %s`",
					entryLabel(entry), reason, (now-entry.LastStatusAt)/3600.0, p.provider))
				prune = append(prune, entry.ID)
				clearedAny = true
			}
			continue
		}
		if _, cooled := ModelCooldownUntil(entry, model); cooled {
			continue
		}
		if entry.LastStatus == StatusExhausted {
			until, ok := exhaustedUntil(entry, sole)
			if ok && now < until && !(clearExpired && p.codexQuotaRestoredUpstream(entry, true)) {
				continue
			}
			if clearExpired {
				if live, found := p.findByID(entry.ID); found {
					entry = live
				}
				entry = p.adopt(entry.MarkOK(), true, false)
				clearedAny = true
			}
		}
		if refresh && p.entryNeedsRefresh(entry) {
			if _, single := tokensSingletonProviders[p.provider]; single {
				pending = append(pending, entry)
				continue
			}
			refreshed, ok := p.refreshEntry(entry, false, "", true)
			if !ok {
				continue
			}
			entry = refreshed
		}
		if entry.AuthType == AuthTypeOAuth && strings.TrimSpace(entry.AccessToken) == "" {
			continue
		}
		available = append(available, entry)
	}
	if len(prune) > 0 {
		pruned := map[string]bool{}
		for _, id := range prune {
			pruned[id] = true
		}
		kept := p.entries[:0:0]
		for _, e := range p.entries {
			if !pruned[e.ID] {
				kept = append(kept, e)
			}
		}
		p.entries = kept
	}
	if clearedAny {
		p.persist(true, prune, nil)
	}
	return available, pending
}

// Row identity used to detect a resync, comparing the persisted shape rather than the pointer
func sameRow(a, b PooledCredential) bool {
	return a.ID == b.ID && a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken && a.LastStatus == b.LastStatus &&
		a.LastStatusAt == b.LastStatusAt && a.StatusClearedAt == b.StatusClearedAt && a.AgentKey == b.AgentKey &&
		a.ExpiresAt == b.ExpiresAt && a.ExpiresAtMS == b.ExpiresAtMS && a.LastRefresh == b.LastRefresh && a.AgentKeyExpiresAt == b.AgentKeyExpiresAt
}

// Emits the empty pool INFO line at most once per throttle window
func (p *Pool) logNoAvailableEntries() {
	now := nowFunc()
	if !p.lastNoEntriesLogAt.IsZero() && now.Sub(p.lastNoEntriesLogAt) < NoAvailableEntriesLogThrottle {
		return
	}
	p.lastNoEntriesLogAt = now
	p.env.log().Info("credential pool: no available entries (all exhausted or empty)")
}

// Selects the best available entry under the lock and returns the deferred refresh rows
func (p *Pool) selectUnlocked(refresh, count bool, model string) (PooledCredential, bool, []PooledCredential) {
	available, pending := p.availableEntries(true, refresh, model)
	if len(available) == 0 {
		p.currentID = ""
		p.logNoAvailableEntries()
		return PooledCredential{}, false, pending
	}
	p.lastNoEntriesLogAt = time.Time{}
	var entry PooledCredential
	switch {
	case p.strategy == StrategyRandom:
		entry = available[rand.Intn(len(available))]
	case p.strategy == StrategyLeastUsed && len(available) > 1:
		entry = available[0]
		for _, c := range available[1:] {
			if c.RequestCount < entry.RequestCount {
				entry = c
			}
		}
	default:
		entry = available[0]
	}
	if count {
		u := entry.Clone()
		u.RequestCount++
		entry = p.adopt(u, true, false)
	}
	if p.strategy == StrategyRoundRobin && len(available) > 1 {
		rotated := make([]PooledCredential, 0, len(p.entries))
		for _, c := range p.entries {
			if c.ID != entry.ID {
				rotated = append(rotated, c)
			}
		}
		last := entry.Clone()
		last.Priority = len(p.entries) - 1
		rotated = append(rotated, last)
		for i := range rotated {
			rotated[i].Priority = i
		}
		p.entries = rotated
		p.persist(true, nil, nil)
		if live, ok := p.findByID(entry.ID); ok {
			entry = live
		}
	}
	p.currentID = entry.ID
	return entry, true, pending
}

func (p *Pool) selectUnderLock(model string) (PooledCredential, bool, []PooledCredential) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.selectUnlocked(true, true, model)
}

// Refreshes deferred single use rows outside the pool lock with a spacing window between holds
func (p *Pool) refreshPendingEntries(pending []PooledCredential) {
	for i, entry := range pending {
		if i > 0 {
			sleepFunc(refreshSweepSpacing)
		}
		p.refreshEntry(entry, false, "", false)
	}
}

var sleepFunc = time.Sleep

// Select picks the next usable credential for model, refreshing deferred rows then reselecting
func (p *Pool) Select(model string) (PooledCredential, bool) {
	entry, ok, pending := p.selectUnderLock(model)
	if len(pending) > 0 {
		p.refreshPendingEntries(pending)
		if !ok {
			entry, ok, _ = p.selectUnderLock(model)
		}
	}
	if ok {
		p.mu.Lock()
		p.unmatchedRotationStreak = 0
		p.mu.Unlock()
	}
	return entry, ok
}

// Peek is the current entry or the first available one without selecting
func (p *Pool) Peek() (PooledCredential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.currentLocked(); ok {
		return cur, true
	}
	available, _ := p.availableEntries(false, false, "")
	if len(available) == 0 {
		return PooledCredential{}, false
	}
	return available[0], true
}

// Reclaim hands back the entry once its cooldown lifted without counting a request
func (p *Pool) Reclaim(credentialID, model string) (PooledCredential, bool) {
	p.mu.Lock()
	available, pending := p.availableEntries(true, true, model)
	p.mu.Unlock()
	var mine []PooledCredential
	for _, e := range pending {
		if e.ID == credentialID {
			mine = append(mine, e)
		}
	}
	if len(mine) > 0 {
		p.refreshPendingEntries(mine)
		p.mu.Lock()
		available, _ = p.availableEntries(true, true, model)
		p.mu.Unlock()
	}
	for _, e := range available {
		if e.ID == credentialID {
			return e, true
		}
	}
	return PooledCredential{}, false
}

// ---- rotation ----

// Resolves the entry that issued a failed request from its supplied identity
func (p *Pool) identifyFailedEntry(credentialID, apiKeyHint string) (PooledCredential, bool) {
	var entry PooledCredential
	found := false
	if credentialID != "" {
		entry, found = p.findByID(credentialID)
		if found && apiKeyHint != "" && entry.RuntimeAPIKey() != apiKeyHint {
			hint, hintFound := p.findLocked(func(e PooledCredential) bool { return e.RuntimeAPIKey() == apiKeyHint })
			if hintFound {
				p.env.log().Info(fmt.Sprintf("credential pool: credential_id %s runtime key does not match api_key_hint; attributing failure to key-matched entry %s instead (#79156)", entryLabel(entry), entryLabel(hint)))
			}
			entry, found = hint, hintFound
		}
	}
	if !found && apiKeyHint != "" {
		entry, found = p.findLocked(func(e PooledCredential) bool { return e.RuntimeAPIKey() == apiKeyHint })
	}
	return entry, found
}

// Rotates without marking anything when the failed identity matches no entry, bounded to one lap
func (p *Pool) rotateUnmatched() (PooledCredential, bool) {
	p.unmatchedRotationStreak++
	available, _ := p.availableEntries(false, false, "")
	cap := len(available)
	if cap < 1 {
		cap = 1
	}
	if p.unmatchedRotationStreak > cap {
		p.env.log().Warn(fmt.Sprintf("credential pool: failed credential identity matched no %s entry for %d consecutive rotations (pool size %d) — surfacing the error instead of rotating again", p.provider, p.unmatchedRotationStreak, len(available)))
		p.unmatchedRotationStreak = 0
		p.currentID = ""
		return PooledCredential{}, false
	}
	p.env.log().Info(fmt.Sprintf("credential pool: failed credential identity matched no %s entry; rotating without marking any credential exhausted", p.provider))
	p.currentID = ""
	next, ok, _ := p.selectUnlocked(false, true, "")
	if ok {
		if nowAvailable, _ := p.availableEntries(false, false, ""); len(nowAvailable) == 1 {
			p.unmatchedRotationStreak = 0
			p.currentID = ""
			return PooledCredential{}, false
		}
	}
	return next, ok
}

// RotationRequest identifies a failed request and its classified failure
type RotationRequest struct {
	StatusCode    int
	ErrorContext  map[string]any
	APIKeyHint    string
	CredentialID  string
	FailureReason string
	Model         string
}

// MarkExhaustedAndRotate benches the failed credential and returns the next usable entry
func (p *Pool) MarkExhaustedAndRotate(req RotationRequest) (PooledCredential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	identitySupplied := req.CredentialID != "" || req.APIKeyHint != ""
	entry, found := p.identifyFailedEntry(req.CredentialID, req.APIKeyHint)
	if !found && identitySupplied {
		return p.rotateUnmatched()
	}
	p.unmatchedRotationStreak = 0
	if !found {
		entry, found = p.currentLocked()
		if !found {
			entry, found, _ = p.selectUnlocked(false, true, "")
		}
	}
	if !found {
		return PooledCredential{}, false
	}
	label := entryLabel(entry)
	if p.isModelScopedFailure(req.StatusCode, req.Model, req.FailureReason) {
		p.coolDownModel(entry, req.Model, req.ErrorContext, req.FailureReason)
		p.env.log().Info(fmt.Sprintf("credential pool: %s unavailable for model %s; other models stay available", label, req.Model))
		p.currentID = ""
		next, ok, _ := p.selectUnlocked(false, true, req.Model)
		return next, ok
	}
	p.markExhausted(entry, req.StatusCode, req.ErrorContext, true, req.FailureReason, true)
	failedKey := entry.RuntimeAPIKey()
	if identitySupplied && failedKey != "" {
		siblings := 0
		for _, s := range append([]PooledCredential(nil), p.entries...) {
			if s.ID != entry.ID && s.RuntimeAPIKey() == failedKey {
				p.markExhausted(s, req.StatusCode, req.ErrorContext, false, req.FailureReason, true)
				siblings++
			}
		}
		if siblings > 0 {
			p.persist(true, nil, nil)
		}
	}
	updated := entry
	if live, ok := p.findByID(entry.ID); ok {
		updated = live
	}
	if updated.LastStatus == StatusDead {
		reason := updated.LastErrorReason
		if reason == "" {
			reason = "unknown"
		}
		p.env.log().Warn(fmt.Sprintf("credential pool: marking %s DEAD (status=%d, reason=%s) — permanently failed, will NOT re-enter rotation until re-auth", label, req.StatusCode, reason))
	} else {
		p.env.log().Info(fmt.Sprintf("credential pool: marking %s exhausted (status=%d), rotating", label, req.StatusCode))
	}
	p.currentID = ""
	next, ok, _ := p.selectUnlocked(false, true, "")
	if ok && next.ID == entry.ID {
		p.env.log().Warn(fmt.Sprintf("credential pool: rotation returned the just-marked entry %s — treating as no-recovery so the failure surfaces", label))
		p.currentID = ""
		return PooledCredential{}, false
	}
	if ok {
		p.env.log().Info(fmt.Sprintf("credential pool: rotated to %s", entryLabel(next)))
	}
	return next, ok
}

// ---- leases ----

// AcquireLease takes a soft lease on the given entry, else the least leased available one
func (p *Pool) AcquireLease(credentialID string) (string, bool) {
	chosen, ok, pending := p.acquireLeaseUnderLock(credentialID)
	if len(pending) > 0 {
		p.refreshPendingEntries(pending)
		if !ok {
			chosen, ok, _ = p.acquireLeaseUnderLock(credentialID)
		}
	}
	return chosen, ok
}

func (p *Pool) acquireLeaseUnderLock(credentialID string) (string, bool, []PooledCredential) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if credentialID != "" {
		p.activeLeases[credentialID]++
		p.currentID = credentialID
		return credentialID, true, nil
	}
	available, pending := p.availableEntries(true, true, "")
	if len(available) == 0 {
		return "", false, pending
	}
	var belowCap []PooledCredential
	for _, e := range available {
		if p.activeLeases[e.ID] < p.maxConcurrent {
			belowCap = append(belowCap, e)
		}
	}
	candidates := belowCap
	if len(candidates) == 0 {
		candidates = available
	}
	chosen := candidates[0]
	for _, c := range candidates[1:] {
		if p.activeLeases[c.ID] < p.activeLeases[chosen.ID] || (p.activeLeases[c.ID] == p.activeLeases[chosen.ID] && c.Priority < chosen.Priority) {
			chosen = c
		}
	}
	p.activeLeases[chosen.ID]++
	p.currentID = chosen.ID
	return chosen.ID, true, pending
}

// ActiveLeases copies the live lease count per credential id for tests and diagnostics
func (p *Pool) ActiveLeases() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.activeLeases))
	for k, v := range p.activeLeases {
		out[k] = v
	}
	return out
}

// ReleaseLease drops one soft lease on the entry
func (p *Pool) ReleaseLease(credentialID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.activeLeases[credentialID] <= 1 {
		delete(p.activeLeases, credentialID)
		return
	}
	p.activeLeases[credentialID]--
}

// ---- explicit refresh ----

// TryRefreshCurrent force refreshes the entry selected last
func (p *Pool) TryRefreshCurrent() (PooledCredential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tryRefreshCurrentUnlocked("")
}

// TryRefreshMatching force refreshes the entry that supplied the failed request
func (p *Pool) TryRefreshMatching(apiKeyHint, credentialID string) (PooledCredential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var entry PooledCredential
	found := false
	if credentialID != "" {
		entry, found = p.findByID(credentialID)
	}
	if !found {
		if apiKeyHint != "" {
			entry, found = p.findLocked(func(e PooledCredential) bool { return e.RuntimeAPIKey() == apiKeyHint })
		} else {
			entry, found = p.currentLocked()
			if !found {
				entry, found, _ = p.selectUnlocked(false, false, "")
			}
		}
	}
	if !found {
		return PooledCredential{}, false
	}
	p.currentID = entry.ID
	return p.tryRefreshCurrentUnlocked(apiKeyHint)
}

func (p *Pool) tryRefreshCurrentUnlocked(failedKey string) (PooledCredential, bool) {
	entry, ok := p.currentLocked()
	if !ok {
		return PooledCredential{}, false
	}
	refreshed, ok := p.refreshEntry(entry, true, failedKey, true)
	if ok {
		p.currentID = refreshed.ID
	}
	return refreshed, ok
}

// ---- model scoped cooldowns ----

// TokenIsBlocked reports whether a pool cooldown blocks the token for model, unknown tokens fail open
func (p *Pool) TokenIsBlocked(token, model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.RuntimeAPIKey() == token {
			if _, cooled := ModelCooldownUntil(e, model); cooled {
				return true
			}
		}
	}
	return false
}

// Anthropic per model 429s and Codex entitlement rejections bench one model and keep the credential
func (p *Pool) isModelScopedFailure(statusCode int, model, failureReason string) bool {
	if model == "" {
		return false
	}
	if failureReason == string(ReasonModelEntitlement) || failureReason == FailureReasonUsageWindow {
		return true
	}
	return p.provider == "anthropic" && statusCode == 429 && failureReason != FailureReasonBilling && failureReason != FailureReasonBillingUnverified
}

// Records a cooldown for model on the entry and every sibling sharing its key, caller holds the lock
func (p *Pool) coolDownModel(entry PooledCredential, model string, errorContext map[string]any, failureReason string) {
	var until float64
	if failureReason == string(ReasonModelEntitlement) {
		until = nowEpoch() + ModelEntitlementBench.Seconds()
	} else if reset, ok := anyFloat(normalizeErrorContext(errorContext)["reset_at"]); ok && reset != 0 {
		until = reset
	} else {
		until = nowEpoch() + exhaustedTTL(429, p.isSoleCredential(), "").Seconds()
	}
	failedKey := entry.RuntimeAPIKey()
	for _, scoped := range append([]PooledCredential(nil), p.entries...) {
		if scoped.ID != entry.ID && !(failedKey != "" && scoped.RuntimeAPIKey() == failedKey) {
			continue
		}
		u := scoped.Clone()
		u.ModelCooldowns = MergeModelCooldowns(scoped.ModelCooldowns, map[string]float64{model: until})
		p.adopt(u, true, false)
	}
	p.persist(true, nil, nil)
}

// LimitState describes a pool wide or per model rate limit a picker should mention
type LimitState struct {
	Scope    string
	ResetsAt float64
	Models   map[string]float64
}

// LimitState reports an account wide bench or the model cooldowns blocking the given models, nil when none
func (p *Pool) LimitState(models []string) *LimitState {
	now := nowEpoch()
	p.mu.Lock()
	defer p.mu.Unlock()
	var live []PooledCredential
	for _, e := range p.entries {
		if e.LastStatus != StatusDead {
			live = append(live, e)
		}
	}
	sole := len(live) <= 1
	benched := map[string]float64{}
	for _, e := range live {
		if until, ok := exhaustedUntil(e, sole); ok && until > now {
			benched[e.ID] = until
		}
	}
	if len(live) > 0 && len(benched) == len(live) {
		min := 0.0
		first := true
		for _, until := range benched {
			if first || until < min {
				min, first = until, false
			}
		}
		return &LimitState{Scope: "account", ResetsAt: min}
	}
	var usable []PooledCredential
	for _, e := range live {
		if _, isBenched := benched[e.ID]; !isBenched {
			usable = append(usable, e)
		}
	}
	cooled := map[string]float64{}
	for _, model := range models {
		if len(usable) == 0 {
			continue
		}
		min, all := 0.0, true
		for i, e := range usable {
			until, ok := ModelCooldownUntil(e, model)
			if !ok {
				all = false
				break
			}
			if i == 0 || until < min {
				min = until
			}
		}
		if all && min-now < ModelEntitlementBench.Seconds()/2 {
			cooled[model] = min
		}
	}
	if len(cooled) == 0 {
		return nil
	}
	return &LimitState{Scope: "models", Models: cooled}
}

// ---- Rotator and CredentialSource contracts ----

// Credential is the pool's current login for the provider when the pool serves it
func (p *Pool) Credential(provider string) (Credential, bool) {
	return p.CredentialForModel(provider, "")
}

// CredentialForModel selects a login not benched for the model
func (p *Pool) CredentialForModel(provider, model string) (Credential, bool) {
	if !CredentialPoolMatchesProviderIn(p.provider, provider, "", p.env.config()) {
		return Credential{}, false
	}
	entry, ok := p.Select(model)
	if !ok {
		return Credential{}, false
	}
	return poolCredential(p, entry), true
}

// TokenBenchedForModel reports a model cooldown on the token
func (p *Pool) TokenBenchedForModel(provider, token, model string) bool {
	if !CredentialPoolMatchesProviderIn(p.provider, provider, "", p.env.config()) {
		return false
	}
	return p.TokenIsBlocked(token, model)
}

func poolCredential(pool *Pool, entry PooledCredential) Credential {
	return Credential{APIKey: entry.RuntimeAPIKey(), BaseURL: entry.RuntimeBaseURL(), Source: "pool:" + entry.Source, Pool: pool}
}

// Rotate benches the route's credential for the classified failure and rebinds the route to the next entry
func (p *Pool) Rotate(ctx context.Context, route Route, apiErr *APIError) (Route, bool) {
	if apiErr == nil {
		return route, false
	}
	provider := strings.ToLower(strings.TrimSpace(route.Provider))
	if p.provider != "" && !CredentialPoolMatchesProviderIn(p.provider, provider, route.BaseURL, p.env.config()) {
		p.env.log().Warn(fmt.Sprintf("Credential pool provider mismatch: pool=%s, agent=%s — skipping pool mutation to avoid cross-provider contamination", p.provider, provider))
		return route, false
	}
	cl := ClassifyError(ClassifyInput{Err: apiErr, Provider: route.Provider, Model: route.Model, BaseURL: route.BaseURL, APIKey: route.APIKey})
	if !cl.ShouldRotateCredential {
		return route, false
	}
	errorContext := ExtractAPIErrorContext(apiErr)
	status := apiErr.StatusCode
	if status == 0 {
		status = cl.StatusCode
	}
	req := RotationRequest{StatusCode: status, ErrorContext: errorContext, APIKeyHint: route.APIKey,
		CredentialID: p.EntryIDForAPIKey(route.APIKey), Model: strings.TrimSpace(route.Model),
		FailureReason: PoolFailureReason(cl.Reason, errorContext, cl.BillingUnverified())}
	next, ok := p.MarkExhaustedAndRotate(req)
	if !ok {
		return route, false
	}
	if !CredentialPoolEntryServesEndpoint(next, route.BaseURL) {
		p.env.log().Info(fmt.Sprintf("Credential %d (%s) — pool entry %s serves another endpoint; not swapping", status, cl.Reason, next.ID))
		return route, false
	}
	p.env.log().Info(fmt.Sprintf("Credential %d (%s) — rotated to pool entry %s", status, cl.Reason, next.ID))
	swapped, ok := SwapCredentialRoute(route, next)
	return swapped, ok
}

// SwapCredentialRoute rebinds a route to a pool entry, refusing when the entry's route cannot serve the model
func SwapCredentialRoute(route Route, entry PooledCredential) (Route, bool) {
	runtimeKey := entry.RuntimeAPIKey()
	if runtimeKey == "" {
		runtimeKey = entry.AccessToken
	}
	runtimeBase := entry.RuntimeBaseURL()
	if runtimeBase == "" {
		runtimeBase = entry.BaseURL
	}
	if runtimeBase == "" {
		runtimeBase = route.BaseURL
	}
	actual := IsActualRoute(route.Provider, runtimeBase)
	if actual {
		runtimeBase = NormalizeActualBaseURL(runtimeBase)
	}
	runtimeBase = strings.TrimRight(runtimeBase, "/")
	if !routeCanServeModel(route.Provider, runtimeBase, route.Model) {
		return route, false
	}
	next := route
	if actual {
		next.Mode = ModeChatCompletions
	}
	next.APIKey = runtimeKey
	next.BaseURL = runtimeBase
	next.TokenSource = nil
	return next, true
}

// CredentialPoolEntryServesEndpoint reports whether a pooled credential may be bound to a session at baseURL
func CredentialPoolEntryServesEndpoint(entry PooledCredential, baseURL string) bool {
	if baseURL == "" {
		return true
	}
	entryURL := entry.RuntimeBaseURL()
	if entryURL == "" {
		entryURL = entry.BaseURL
	}
	if entryURL == "" {
		return true
	}
	return NormalizeRouteBaseURL(entryURL) == NormalizeRouteBaseURL(baseURL)
}
