package auth

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

var terminalAuthReasons = []string{
	"token_invalidated", "token_revoked", "invalid_token", "invalid_grant", "unauthorized_client", "refresh_token_reused",
}

const noAvailableEntriesLogThrottle = 60 * time.Second

type Failure struct {
	Status       int
	Reason       string
	ErrorReason  string
	Message      string
	ResetAt      time.Time
	Model        string
	APIKeyHint   Secret
	CredentialID string
}

type poolHeldKey struct{ p *Pool }

type Pool struct {
	s        *Store
	provider string
	spec     SeedSpec
	strategy config.PoolStrategy
	rt       *entryRuntime

	mu              sync.Mutex
	entries         []*Entry
	currentID       string
	borrowedRootIDs []string
	persistedPairs  map[string]tokenPair
	leases          map[string]int
	lastNoEntries   time.Time
	unmatchedStreak int
}

func (p *Pool) lock(ctx context.Context) (context.Context, func()) {
	if ctx.Value(poolHeldKey{p}) != nil {
		return ctx, func() {}
	}
	p.mu.Lock()
	return context.WithValue(ctx, poolHeldKey{p}, true), p.mu.Unlock
}

func (s *Store) newPool(ctx context.Context, provider string, entries []*Entry) *Pool {
	p := &Pool{s: s, provider: provider, strategy: config.PoolFillFirst, persistedPairs: map[string]tokenPair{}, leases: map[string]int{}}
	p.spec, _ = s.dir.Spec(ctx, provider)
	p.rt = &entryRuntime{key: p.spec.RuntimeKey, codexBase: func(ctx context.Context) string {
		return strings.TrimRight(strings.TrimSpace(s.lookupEnv(s.bind(ctx), "ICEBATH_CODEX_BASE_URL")), "/")
	}}
	if cfg, err := s.cfg.LoadReadonly(ctx); err == nil {
		if strategy, ok := cfg.CredentialPoolStrategies.Get(provider); ok {
			normalized := config.PoolStrategy(strings.ToLower(strings.TrimSpace(string(strategy))))
			if slices.Contains(config.PoolStrategies(), normalized) {
				p.strategy = normalized
			}
		}
	}
	for _, e := range entries {
		e.rt = p.rt
	}
	p.entries = slices.Clone(entries)
	sort.SliceStable(p.entries, func(i, j int) bool { return p.entries[i].Priority < p.entries[j].Priority })
	return p
}

func (p *Pool) Provider() string { return p.provider }

func (p *Pool) Strategy() config.PoolStrategy { return p.strategy }

func (p *Pool) HasCredentials() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries) > 0
}

func (p *Pool) Entries() []Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Entry, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, *e.clone())
	}
	return out
}

func (p *Pool) HasAvailable(ctx context.Context, model string) (bool, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	available, _, err := p.availableEntries(ctx, false, false, model)
	if err != nil {
		return false, err
	}
	return len(available) > 0, nil
}

func (p *Pool) LiftReopenedCooldowns(ctx context.Context, model string) (bool, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	available, _, err := p.availableEntries(ctx, true, false, model)
	if err != nil {
		return false, err
	}
	return len(available) > 0, nil
}

func (p *Pool) NextAvailableAt(ctx context.Context, model string) (time.Time, bool, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	available, _, err := p.availableEntries(ctx, false, false, model)
	if err != nil {
		return time.Time{}, false, err
	}
	if len(available) > 0 {
		return time.Time{}, false, nil
	}
	sole := p.isSoleCredential()
	best, found := 0.0, false
	consider := func(until float64, ok bool) {
		if ok && (!found || until < best) {
			best, found = until, true
		}
	}
	for _, e := range p.entries {
		if e.LastStatus == StatusExhausted {
			consider(exhaustedUntil(e, sole))
		}
		if e.LastStatus != StatusDead {
			consider(modelCooldownUntil(e, model))
		}
	}
	if !found {
		return time.Time{}, false, nil
	}
	return timeOf(best), true, nil
}

func (p *Pool) isSoleCredential() bool {
	alive := 0
	for _, e := range p.entries {
		if e.LastStatus != StatusDead {
			alive++
		}
	}
	return alive <= 1
}

func (p *Pool) find(pred func(e *Entry) bool) *Entry {
	for _, e := range p.entries {
		if pred(e) {
			return e
		}
	}
	return nil
}

func (p *Pool) byID(id string) *Entry {
	if id == "" {
		return nil
	}
	return p.find(func(e *Entry) bool { return e.ID == id })
}

func (p *Pool) byKey(key string) *Entry {
	return p.find(func(e *Entry) bool { return e.RuntimeAPIKey().Reveal() == key })
}

func out(e *Entry) (Entry, bool) {
	if e == nil {
		return Entry{}, false
	}
	return *e.clone(), true
}

func outErr(e *Entry, err error) (Entry, bool, error) {
	if err != nil {
		return Entry{}, false, err
	}
	entry, ok := out(e)
	return entry, ok, nil
}

func (p *Pool) Current() (Entry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return out(p.byID(p.currentID))
}

func (p *Pool) EntryIDForAPIKey(hint Secret) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if current := p.byID(p.currentID); current != nil && (hint.Empty() || current.RuntimeAPIKey().Reveal() == hint.Reveal()) {
		return current.ID, true
	}
	if hint.Empty() {
		return "", false
	}
	var match *Entry
	for _, e := range p.entries {
		if e.RuntimeAPIKey().Reveal() == hint.Reveal() {
			if match != nil {
				return "", false
			}
			match = e
		}
	}
	if match == nil {
		return "", false
	}
	return match.ID, true
}

func (p *Pool) replaceEntry(ctx context.Context, old, updated *Entry) {
	_, unlock := p.lock(ctx)
	defer unlock()
	updated.rt = p.rt
	for i, e := range p.entries {
		if e.ID == old.ID {
			p.entries[i] = updated
			return
		}
	}
}

func (p *Pool) persist(ctx context.Context, removed, cleared []string) error {
	ctx, unlock := p.lock(ctx)
	defer unlock()
	payloads := make([]*doc, 0, len(p.entries))
	for _, e := range p.entries {
		payloads = append(payloads, e.toDoc())
	}
	written, ok, err := p.s.persistPoolEntries(ctx, p.provider, payloads, poolWrite{removed: removed, statusCleared: cleared, tokenBases: p.persistedPairs})
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	rows := entryIDs(written)
	p.persistedPairs = tokenPairsByID(written)
	for _, e := range slices.Clone(p.entries) {
		row, pair := rows[e.ID], p.persistedPairs[e.ID]
		if row == nil || !pair.any() {
			continue
		}
		if textOf(pair[0]) != e.AccessToken.Reveal() || textOf(pair[1]) != e.RefreshToken.Reveal() {
			p.replaceEntry(ctx, e, entryFromDoc(p.provider, row))
		}
	}
	return nil
}

func (p *Pool) swap(ctx context.Context, e *Entry, mutate func(u *Entry)) *Entry {
	updated := e.clone()
	mutate(updated)
	p.replaceEntry(ctx, e, updated)
	return updated
}

func (p *Pool) adopt(ctx context.Context, e *Entry, mutate func(u *Entry)) (*Entry, error) {
	ctx, unlock := p.lock(ctx)
	defer unlock()
	updated := p.swap(ctx, e, mutate)
	if err := p.persist(ctx, nil, nil); err != nil {
		return nil, err
	}
	if live := p.byID(updated.ID); live != nil {
		return live, nil
	}
	return updated, nil
}

func clearStatus(e *Entry) {
	e.LastStatus, e.LastStatusAt, e.statusAt = "", time.Time{}, 0
	e.LastErrorCode, e.LastErrorReason, e.LastErrorMessage = 0, "", ""
	e.LastErrorResetAt, e.resetAt = time.Time{}, 0
}

func markOK(e *Entry) {
	clearStatus(e)
	e.LastStatus = StatusOK
}

func stampStatus(e *Entry, status string) {
	e.LastStatus = status
	e.LastStatusAt = time.Now()
	e.statusAt = epochOf(e.LastStatusAt)
}

func (p *Pool) quarantineSources(ctx context.Context, entry *Entry, sources []string) error {
	ctx, unlock := p.lock(ctx)
	defer unlock()
	removed := []string{}
	kept := []*Entry{}
	for _, e := range p.entries {
		if slices.Contains(sources, e.Source) {
			removed = append(removed, e.ID)
		} else {
			kept = append(kept, e)
		}
	}
	p.entries = kept
	if p.currentID == entry.ID {
		p.currentID = ""
	}
	return p.persist(ctx, removed, nil)
}

func isTerminalAuthFailure(status int, reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason == ReasonPersistFailed {
		return true
	}
	return status == 401 && slices.Contains(terminalAuthReasons, reason)
}

func (p *Pool) markExhausted(ctx context.Context, e *Entry, f Failure, persist bool) (*Entry, error) {
	reason, message := strings.TrimSpace(f.ErrorReason), strings.TrimSpace(f.Message)
	terminal := e.LastStatus == StatusDead || isTerminalAuthFailure(f.Status, reason)
	mutate := func(u *Entry) {
		if f.Reason != "" {
			u.setExtra("failure_reason", f.Reason)
		} else {
			u.setExtra("failure_reason", nil)
		}
		if terminal {
			stampStatus(u, StatusDead)
		} else {
			stampStatus(u, StatusExhausted)
		}
		u.LastErrorCode, u.LastErrorReason, u.LastErrorMessage = f.Status, reason, message
		u.LastErrorResetAt, u.resetAt = f.ResetAt, 0
	}
	if !persist {
		return p.swap(ctx, e, mutate), nil
	}
	return p.adopt(ctx, e, mutate)
}

func (p *Pool) logNoAvailableEntries() {
	if !p.lastNoEntries.IsZero() && time.Since(p.lastNoEntries) < noAvailableEntriesLogThrottle {
		return
	}
	p.lastNoEntries = time.Now()
	slog.Info("credential pool: no available entries (all exhausted or empty)")
}

func labelOf(e *Entry) string {
	if e.Label != "" {
		return e.Label
	}
	return e.ID[:min(len(e.ID), 8)]
}

func (p *Pool) resetClearedAfter(ctx context.Context, e *Entry) (float64, bool) {
	rows, err := p.s.readPool(ctx, p.provider)
	if err != nil {
		slog.Debug(fmt.Sprintf("Pool entry %s: could not read reset marker", e.ID), "error", err)
		return 0, false
	}
	for _, row := range rows {
		if rowID(row) == e.ID {
			cleared, ok := parseAbsolute(row.value("status_cleared_at"))
			return cleared, ok && cleared != 0 && cleared > e.statusEpoch()
		}
	}
	return 0, false
}

var resyncSource = map[string]string{
	ProviderAnthropic: sourceClaudeCode, ProviderPortal: "device_code", ProviderCodex: "device_code", ProviderXAIOAuth: "device_code",
}

func (p *Pool) resyncStaleEntry(ctx context.Context, e *Entry) *Entry {
	if e.LastStatus != StatusExhausted && e.LastStatus != StatusDead {
		return e
	}
	if cleared, ok := p.resetClearedAfter(ctx, e); ok {
		return p.swap(ctx, e, func(u *Entry) {
			markOK(u)
			u.StatusClearedAt, u.clearedAt = timeOf(cleared), cleared
		})
	}
	if source, ok := resyncSource[p.provider]; !ok || e.Source != source {
		return e
	}
	switch p.provider {
	case ProviderAnthropic:
		return p.syncAnthropicFromCredentialsFile(ctx, e)
	case ProviderPortal:
		return p.syncPortalFromAuthStore(ctx, e)
	}
	return p.syncFromAuthStore(ctx, e)
}

func (p *Pool) availableEntries(ctx context.Context, clearExpired, refresh bool, model string) (available, pending []*Entry, err error) {
	now := nowEpoch()
	clearedAny := false
	prune := []string{}
	sole := p.isSoleCredential()
	scanned := p.entries
	for i := 0; i < len(scanned); i++ {
		entry := scanned[i]
		if entry.AuthType == AuthTypeAPIKey && entry.RuntimeAPIKey().Empty() {
			continue
		}
		if synced := p.resyncStaleEntry(ctx, entry); synced != entry {
			entry, clearedAny = synced, true
		}
		if entry.LastStatus == StatusDead {
			if deadAt := entry.statusEpoch(); isManualSource(entry.Source) && deadAt != 0 && now-deadAt > deadManualPruneTTL {
				reason := entry.LastErrorReason
				if reason == "" {
					reason = "unknown"
				}
				slog.Warn(fmt.Sprintf("credential pool: pruning DEAD manual entry %s (reason=%s, age=%.1fh) — re-add via `icebath auth add %s`",
					labelOf(entry), reason, (now-deadAt)/3600.0, p.provider))
				prune = append(prune, entry.ID)
				clearedAny = true
			}
			continue
		}
		if _, cooling := modelCooldownUntil(entry, model); cooling {
			continue
		}
		if entry.LastStatus == StatusExhausted {
			until, ok := exhaustedUntil(entry, sole)
			if ok && now < until && !(clearExpired && p.codexQuotaRestoredUpstream(ctx, entry)) {
				continue
			}
			if clearExpired {
				if live := p.byID(entry.ID); live != nil {
					entry = live
				}
				entry = p.swap(ctx, entry, markOK)
				clearedAny = true
			}
		}
		if refresh && p.entryNeedsRefresh(entry) {
			if p.provider == ProviderCodex || p.provider == ProviderXAIOAuth {
				pending = append(pending, entry)
				continue
			}
			refreshed, err := p.refreshEntry(ctx, entry, false, "")
			if err != nil {
				return nil, nil, err
			}
			if refreshed == nil {
				continue
			}
			entry = refreshed
		}
		if entry.AuthType == AuthTypeOAuth && strings.TrimSpace(entry.AccessToken.Reveal()) == "" {
			continue
		}
		available = append(available, entry)
	}
	if len(prune) > 0 {
		p.entries = slices.DeleteFunc(slices.Clone(p.entries), func(e *Entry) bool { return slices.Contains(prune, e.ID) })
	}
	if clearedAny {
		if err := p.persist(ctx, prune, nil); err != nil {
			return nil, nil, err
		}
	}
	return available, pending, nil
}

func (p *Pool) selectUnlocked(ctx context.Context, refresh, count bool, model string) (*Entry, []*Entry, error) {
	available, pending, err := p.availableEntries(ctx, true, refresh, model)
	if err != nil {
		return nil, nil, err
	}
	if len(available) == 0 {
		p.currentID = ""
		p.logNoAvailableEntries()
		return nil, pending, nil
	}
	p.lastNoEntries = time.Time{}
	entry := available[0]
	switch {
	case p.strategy == config.PoolRandom:
		entry = available[rand.IntN(len(available))]
	case p.strategy == config.PoolLeastUsed && len(available) > 1:
		for _, e := range available[1:] {
			if e.RequestCount < entry.RequestCount {
				entry = e
			}
		}
	}
	if count {
		entry = p.swap(ctx, entry, func(u *Entry) { u.RequestCount++ })
	}
	if p.strategy == config.PoolRoundRobin && len(available) > 1 {
		rotated := slices.DeleteFunc(slices.Clone(p.entries), func(e *Entry) bool { return e.ID == entry.ID })
		rotated = append(rotated, entry)
		for i, e := range rotated {
			u := e.clone()
			u.Priority = i
			rotated[i] = u
		}
		p.entries = rotated
		if err := p.persist(ctx, nil, nil); err != nil {
			return nil, nil, err
		}
		if live := p.byID(entry.ID); live != nil {
			entry = live
		}
	}
	p.currentID = entry.ID
	return entry, pending, nil
}

func (p *Pool) refreshPending(ctx context.Context, pending []*Entry) error {
	for i, e := range pending {
		if i > 0 {
			if err := cancellableSleep(ctx, 500*time.Millisecond, nil); err != nil {
				return err
			}
		}
		if _, err := p.refreshEntry(ctx, e, false, ""); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pool) selectLocked(ctx context.Context, model string) (*Entry, []*Entry, error) {
	ctx, unlock := p.lock(ctx)
	defer unlock()
	return p.selectUnlocked(ctx, true, true, model)
}

func (p *Pool) Select(ctx context.Context, model string) (Entry, bool, error) {
	ctx = p.s.bind(ctx)
	entry, pending, err := p.selectLocked(ctx, model)
	if err != nil {
		return Entry{}, false, err
	}
	if len(pending) > 0 {
		if err := p.refreshPending(ctx, pending); err != nil {
			return Entry{}, false, err
		}
		if entry == nil {
			if entry, _, err = p.selectLocked(ctx, model); err != nil {
				return Entry{}, false, err
			}
		}
	}
	if entry != nil {
		p.mu.Lock()
		p.unmatchedStreak = 0
		p.mu.Unlock()
	}
	return outErr(entry, nil)
}

func (p *Pool) Peek(ctx context.Context) (Entry, bool, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	if current := p.byID(p.currentID); current != nil {
		return outErr(current, nil)
	}
	available, _, err := p.availableEntries(ctx, false, false, "")
	if err != nil || len(available) == 0 {
		return outErr(nil, err)
	}
	return outErr(available[0], nil)
}

func (p *Pool) Reclaim(ctx context.Context, id, model string) (Entry, bool, error) {
	ctx = p.s.bind(ctx)
	scan := func() ([]*Entry, []*Entry, error) {
		lctx, unlock := p.lock(ctx)
		defer unlock()
		return p.availableEntries(lctx, true, true, model)
	}
	available, pending, err := scan()
	if err != nil {
		return Entry{}, false, err
	}
	if mine := slices.DeleteFunc(slices.Clone(pending), func(e *Entry) bool { return e.ID != id }); len(mine) > 0 {
		if err := p.refreshPending(ctx, mine); err != nil {
			return Entry{}, false, err
		}
		if available, _, err = scan(); err != nil {
			return Entry{}, false, err
		}
	}
	for _, e := range available {
		if e.ID == id {
			return outErr(e, nil)
		}
	}
	return Entry{}, false, nil
}

func (p *Pool) identifyFailedEntry(id, hint string) *Entry {
	var entry *Entry
	if id != "" {
		entry = p.byID(id)
		if entry != nil && hint != "" && entry.RuntimeAPIKey().Reveal() != hint {
			hintEntry := p.byKey(hint)
			if hintEntry != nil {
				slog.Info(fmt.Sprintf("credential pool: credential_id %s runtime key does not match api_key_hint; "+
					"attributing failure to key-matched entry %s instead", labelOf(entry), labelOf(hintEntry)))
			}
			entry = hintEntry
		}
	}
	if entry == nil && hint != "" {
		entry = p.byKey(hint)
	}
	return entry
}

func (p *Pool) rotateUnmatched(ctx context.Context) (*Entry, error) {
	p.unmatchedStreak++
	available, _, err := p.availableEntries(ctx, false, false, "")
	if err != nil {
		return nil, err
	}
	if p.unmatchedStreak > max(len(available), 1) {
		slog.Warn(fmt.Sprintf("credential pool: failed credential identity matched no %s entry for %d consecutive rotations (pool size %d) — "+
			"surfacing the error instead of rotating again", p.provider, p.unmatchedStreak, len(available)))
		p.unmatchedStreak, p.currentID = 0, ""
		return nil, nil
	}
	slog.Info(fmt.Sprintf("credential pool: failed credential identity matched no %s entry; rotating without marking any credential exhausted", p.provider))
	p.currentID = ""
	next, _, err := p.selectUnlocked(ctx, false, true, "")
	if err != nil {
		return nil, err
	}
	if next != nil {
		again, _, err := p.availableEntries(ctx, false, false, "")
		if err != nil {
			return nil, err
		}
		if len(again) == 1 {
			p.unmatchedStreak, p.currentID = 0, ""
			return nil, nil
		}
	}
	return next, nil
}

func (p *Pool) isModelScopedFailure(f Failure) bool {
	if f.Model == "" {
		return false
	}
	if f.Reason == ReasonModelEntitlement || f.Reason == ReasonUsageWindow {
		return true
	}
	return p.provider == ProviderAnthropic && f.Status == 429 && f.Reason != ReasonBilling && f.Reason != ReasonBillingUnverified
}

func (p *Pool) coolDownModel(ctx context.Context, entry *Entry, f Failure) error {
	until := nowEpoch() + modelEntitlementTTL
	if f.Reason != ReasonModelEntitlement {
		if !f.ResetAt.IsZero() {
			until = epochOf(f.ResetAt)
		} else {
			until = nowEpoch() + exhaustedTTL(429, p.isSoleCredential(), "")
		}
	}
	failedKey := entry.RuntimeAPIKey().Reveal()
	for _, scoped := range slices.Clone(p.entries) {
		if scoped.ID != entry.ID && !(failedKey != "" && scoped.RuntimeAPIKey().Reveal() == failedKey) {
			continue
		}
		one := newDoc()
		one.set(f.Model, floatNum(until))
		p.swap(ctx, scoped, func(u *Entry) { u.cooldowns = mergeModelCooldowns(scoped.cooldowns, one) })
	}
	return p.persist(ctx, nil, nil)
}

func (p *Pool) MarkExhausted(ctx context.Context, f Failure) (Entry, bool, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	hint := f.APIKeyHint.Reveal()
	supplied := f.CredentialID != "" || hint != ""
	entry := p.identifyFailedEntry(f.CredentialID, hint)
	if entry == nil && supplied {
		return outErr(p.rotateUnmatched(ctx))
	}
	p.unmatchedStreak = 0
	if entry == nil {
		if entry = p.byID(p.currentID); entry == nil {
			var err error
			if entry, _, err = p.selectUnlocked(ctx, false, true, ""); err != nil {
				return Entry{}, false, err
			}
		}
	}
	if entry == nil {
		return Entry{}, false, nil
	}
	label := labelOf(entry)
	if p.isModelScopedFailure(f) {
		if err := p.coolDownModel(ctx, entry, f); err != nil {
			return Entry{}, false, err
		}
		slog.Info(fmt.Sprintf("credential pool: %s unavailable for model %s; other models stay available", label, f.Model))
		p.currentID = ""
		next, _, err := p.selectUnlocked(ctx, false, true, f.Model)
		return outErr(next, err)
	}
	failedKey := entry.RuntimeAPIKey().Reveal()
	if _, err := p.markExhausted(ctx, entry, f, true); err != nil {
		return Entry{}, false, err
	}
	if supplied && failedKey != "" {
		marked := false
		for _, sibling := range slices.Clone(p.entries) {
			if sibling.ID != entry.ID && sibling.RuntimeAPIKey().Reveal() == failedKey {
				if _, err := p.markExhausted(ctx, sibling, f, false); err != nil {
					return Entry{}, false, err
				}
				marked = true
			}
		}
		if marked {
			if err := p.persist(ctx, nil, nil); err != nil {
				return Entry{}, false, err
			}
		}
	}
	if updated := p.byID(entry.ID); updated != nil && updated.LastStatus == StatusDead {
		reason := updated.LastErrorReason
		if reason == "" {
			reason = "unknown"
		}
		slog.Warn(fmt.Sprintf("credential pool: marking %s DEAD (status=%d, reason=%s) — permanently failed, will NOT re-enter rotation until re-auth", label, f.Status, reason))
	} else {
		slog.Info(fmt.Sprintf("credential pool: marking %s exhausted (status=%d), rotating", label, f.Status))
	}
	p.currentID = ""
	next, _, err := p.selectUnlocked(ctx, false, true, "")
	if err != nil {
		return Entry{}, false, err
	}
	if next != nil && next.ID == entry.ID {
		slog.Warn(fmt.Sprintf("credential pool: rotation returned the just-marked entry %s — treating as no-recovery so the failure surfaces", label))
		p.currentID = ""
		return Entry{}, false, nil
	}
	if next != nil {
		slog.Info("credential pool: rotated to " + labelOf(next))
	}
	return outErr(next, nil)
}

func (p *Pool) acquireLeaseLocked(ctx context.Context, id string) (string, []*Entry, error) {
	ctx, unlock := p.lock(ctx)
	defer unlock()
	if id != "" {
		p.leases[id]++
		p.currentID = id
		return id, nil, nil
	}
	available, pending, err := p.availableEntries(ctx, true, true, "")
	if err != nil {
		return "", nil, err
	}
	if len(available) == 0 {
		return "", pending, nil
	}
	candidates := slices.DeleteFunc(slices.Clone(available), func(e *Entry) bool { return p.leases[e.ID] >= 1 })
	if len(candidates) == 0 {
		candidates = available
	}
	chosen := candidates[0]
	for _, e := range candidates[1:] {
		if p.leases[e.ID] < p.leases[chosen.ID] || (p.leases[e.ID] == p.leases[chosen.ID] && e.Priority < chosen.Priority) {
			chosen = e
		}
	}
	p.leases[chosen.ID]++
	p.currentID = chosen.ID
	return chosen.ID, pending, nil
}

func (p *Pool) AcquireLease(ctx context.Context, id string) (string, bool, error) {
	ctx = p.s.bind(ctx)
	chosen, pending, err := p.acquireLeaseLocked(ctx, id)
	if err != nil {
		return "", false, err
	}
	if len(pending) > 0 {
		if err := p.refreshPending(ctx, pending); err != nil {
			return "", false, err
		}
		if chosen == "" {
			if chosen, _, err = p.acquireLeaseLocked(ctx, id); err != nil {
				return "", false, err
			}
		}
	}
	return chosen, chosen != "", nil
}

func (p *Pool) ReleaseLease(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.leases[id] <= 1 {
		delete(p.leases, id)
	} else {
		p.leases[id]--
	}
}

func (p *Pool) Lease(ctx context.Context, id string) (release func(), ok bool, err error) {
	chosen, ok, err := p.AcquireLease(ctx, id)
	if err != nil || !ok {
		return func() {}, false, err
	}
	var once sync.Once
	return func() { once.Do(func() { p.ReleaseLease(chosen) }) }, true, nil
}

func (p *Pool) tryRefreshCurrent(ctx context.Context, failedKey string) (*Entry, error) {
	entry := p.byID(p.currentID)
	if entry == nil {
		return nil, nil
	}
	refreshed, err := p.refreshEntry(ctx, entry, true, failedKey)
	if err != nil {
		return nil, err
	}
	if refreshed != nil {
		p.currentID = refreshed.ID
	}
	return refreshed, nil
}

func (p *Pool) TryRefreshCurrent(ctx context.Context) (Entry, bool, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	return outErr(p.tryRefreshCurrent(ctx, ""))
}

func (p *Pool) TryRefreshMatching(ctx context.Context, hint Secret, id string) (Entry, bool, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	entry := p.byID(id)
	if entry == nil {
		if !hint.Empty() {
			entry = p.byKey(hint.Reveal())
		} else if entry = p.byID(p.currentID); entry == nil {
			var err error
			if entry, _, err = p.selectUnlocked(ctx, false, false, ""); err != nil {
				return Entry{}, false, err
			}
		}
	}
	if entry == nil {
		return Entry{}, false, nil
	}
	p.currentID = entry.ID
	return outErr(p.tryRefreshCurrent(ctx, hint.Reveal()))
}

func (p *Pool) TokenIsBlocked(token Secret, model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.RuntimeAPIKey().Reveal() == token.Reveal() {
			if _, cooling := modelCooldownUntil(e, model); cooling {
				return true
			}
		}
	}
	return false
}

func (p *Pool) ModelBlocked(id, model string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byID(id)
	if e == nil {
		return time.Time{}, false
	}
	until, ok := modelCooldownUntil(e, model)
	return timeOf(until), ok
}

type LimitState struct {
	Scope    string
	ResetsAt time.Time
	Models   map[string]time.Time
}

func (p *Pool) LimitState(models []string) (LimitState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := nowEpoch()
	live := []*Entry{}
	for _, e := range p.entries {
		if e.LastStatus != StatusDead {
			live = append(live, e)
		}
	}
	benched := map[string]float64{}
	earliest := 0.0
	for _, e := range live {
		if until, ok := exhaustedUntil(e, len(live) <= 1); ok && until > now {
			benched[e.ID] = until
			if earliest == 0 || until < earliest {
				earliest = until
			}
		}
	}
	if len(live) > 0 && len(benched) == len(live) {
		return LimitState{Scope: "account", ResetsAt: timeOf(earliest)}, true
	}
	cooled := map[string]time.Time{}
	for _, model := range models {
		soonest, all, any := 0.0, true, false
		for _, e := range live {
			if _, isBenched := benched[e.ID]; isBenched {
				continue
			}
			any = true
			until, ok := modelCooldownUntil(e, model)
			if !ok {
				all = false
				break
			}
			if soonest == 0 || until < soonest {
				soonest = until
			}
		}
		if any && all && soonest-now < modelEntitlementTTL/2 {
			cooled[model] = timeOf(soonest)
		}
	}
	if len(cooled) == 0 {
		return LimitState{}, false
	}
	return LimitState{Scope: "models", Models: cooled}, true
}

func clearedStatusCopy(e *Entry) *Entry {
	u := e.clone()
	clearStatus(u)
	u.cooldowns = nil
	u.StatusClearedAt = time.Now()
	u.clearedAt = epochOf(u.StatusClearedAt)
	u.setExtra("failure_reason", nil)
	return u
}

func (p *Pool) ResetStatus(ctx context.Context, id string) (Entry, bool, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	entry := p.byID(id)
	if entry == nil {
		return Entry{}, false, nil
	}
	cleared := clearedStatusCopy(entry)
	p.replaceEntry(ctx, entry, cleared)
	if err := p.persist(ctx, nil, []string{cleared.ID}); err != nil {
		return Entry{}, false, err
	}
	return outErr(cleared, nil)
}

func (p *Pool) ResetStatuses(ctx context.Context) (int, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	ids := []string{}
	for i, e := range p.entries {
		if e.LastStatus != "" || !e.LastStatusAt.IsZero() || e.LastErrorCode != 0 || e.FailureReason() != "" || e.cooldowns.size() > 0 {
			p.entries[i] = clearedStatusCopy(e)
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return len(ids), p.persist(ctx, nil, ids)
}

func (p *Pool) RemoveIndex(ctx context.Context, index int) (Entry, bool, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	if index < 1 || index > len(p.entries) {
		return Entry{}, false, nil
	}
	removed := p.entries[index-1]
	p.entries = slices.Delete(slices.Clone(p.entries), index-1, index)
	for i, e := range p.entries {
		u := e.clone()
		u.Priority = i
		p.entries[i] = u
	}
	if err := p.persist(ctx, []string{removed.ID}, nil); err != nil {
		return Entry{}, false, err
	}
	if p.currentID == removed.ID {
		p.currentID = ""
	}
	return outErr(removed, nil)
}

func (p *Pool) Remove(ctx context.Context, index int) (Removal, bool, error) {
	ctx = p.s.bind(ctx)
	removed, ok, err := p.RemoveIndex(ctx, index)
	if !ok || err != nil {
		return Removal{}, ok, err
	}
	result := Removal{Entry: removed}
	step, found := p.s.findRemovalStep(ctx, p.provider, removed.Source)
	if !found {
		return result, true, nil
	}
	cleaned, err := step.remove(ctx, p.s, p.provider, &removed)
	cleaned.Entry = removed
	if err != nil {
		return cleaned, true, err
	}
	if cleaned.Suppress {
		if err := p.s.Suppress(ctx, p.provider, removed.Source); err != nil {
			return cleaned, true, err
		}
	}
	return cleaned, true, nil
}

func (p *Pool) MoveEntry(ctx context.Context, id string, priority int) (Entry, bool, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	entry := p.byID(id)
	if entry == nil {
		return Entry{}, false, nil
	}
	others := slices.DeleteFunc(slices.Clone(p.entries), func(e *Entry) bool { return e.ID == id })
	others = slices.Insert(others, max(0, min(priority, len(others))), entry)
	for i, e := range others {
		u := e.clone()
		u.Priority = i
		others[i] = u
	}
	normalizePoolPriorities(p.provider, &others)
	sort.SliceStable(others, func(i, j int) bool { return others[i].Priority < others[j].Priority })
	p.entries = others
	if err := p.persist(ctx, nil, nil); err != nil {
		return Entry{}, false, err
	}
	return outErr(p.byID(id), nil)
}

func (p *Pool) ResolveTarget(target string) (index int, entry Entry, message string) {
	raw := strings.TrimSpace(target)
	if raw == "" {
		return 0, Entry{}, "No credential target provided."
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.ID == raw {
			return i + 1, *e.clone(), ""
		}
	}
	matches := []int{}
	for i, e := range p.entries {
		if strings.ToLower(strings.TrimSpace(e.Label)) == strings.ToLower(raw) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 1 {
		return matches[0] + 1, *p.entries[matches[0]].clone(), ""
	}
	if len(matches) > 1 {
		return 0, Entry{}, fmt.Sprintf("Ambiguous credential label \"%s\". Use the numeric index or entry id instead.", raw)
	}
	if n, err := strconv.Atoi(raw); err == nil && strings.Trim(raw, "0123456789") == "" {
		if n >= 1 && n <= len(p.entries) {
			return n, *p.entries[n-1].clone(), ""
		}
		return 0, Entry{}, fmt.Sprintf("No credential #%d.", n)
	}
	return 0, Entry{}, fmt.Sprintf("No credential matching \"%s\".", raw)
}

type CredentialNotSavedError struct {
	Provider string
	Path     string
}

func (e *CredentialNotSavedError) Error() string {
	return fmt.Sprintf("The %s credential was not saved: %s does not contain it after the write, so nothing was added. "+
		"Check that the file is writable, then run the command again.", e.Provider, e.Path)
}

func (p *Pool) Add(ctx context.Context, entry Entry) (Entry, error) {
	ctx, unlock := p.lock(p.s.bind(ctx))
	defer unlock()
	added := entry.clone()
	added.Provider, added.rt = p.provider, p.rt
	if added.ID == "" {
		added.ID = shortID()
	}
	added.AuthType = normalizePoolAuthType(p.provider, added.AccessToken.Reveal(), added.AuthType)
	added.Priority = nextPriority(p.entries)
	p.entries = append(p.entries, added)
	borrows := IsSingleUseRefreshProvider(p.provider) && !p.s.profileOwnsPoolProvider(p.provider) && p.s.rootAuth != ""
	if len(p.borrowedRootIDs) > 0 || borrows {
		p.entries = []*Entry{added}
		written, err := p.s.writePool(ctx, p.provider, []*doc{added.toDoc()}, poolWrite{tokenBases: p.persistedPairs})
		if err != nil {
			return Entry{}, err
		}
		p.persistedPairs = tokenPairsByID(written)
		p.borrowedRootIDs = nil
	} else if err := p.persist(ctx, nil, nil); err != nil {
		return Entry{}, err
	}
	rows, err := p.s.readPool(ctx, p.provider)
	if err != nil {
		return Entry{}, err
	}
	if !slices.ContainsFunc(rows, func(row *doc) bool { return rowID(row) == added.ID }) {
		p.entries = slices.DeleteFunc(slices.Clone(p.entries), func(e *Entry) bool { return e.ID == added.ID })
		return Entry{}, &CredentialNotSavedError{Provider: p.provider, Path: p.s.authPath}
	}
	return *added.clone(), nil
}

func nextPriority(entries []*Entry) int {
	next := 0
	for _, e := range entries {
		if e.Priority+1 > next {
			next = e.Priority + 1
		}
	}
	return next
}

func PoolFailureReason(reason string, usageWindow, billingUnverified bool) string {
	if reason == ReasonBilling && usageWindow {
		return ReasonUsageWindow
	}
	if reason == ReasonBilling && billingUnverified {
		return ReasonBillingUnverified
	}
	return reason
}
