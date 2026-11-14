package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/providers"
)

const (
	refreshAfter = 120 * time.Second
	staleAfter   = 15 * time.Minute
)

type slotKey struct{ provider, identity, baseURL string }

func newSlotKey(provider, identity, baseURL string) slotKey {
	return slotKey{
		provider: strings.ToLower(strings.TrimSpace(provider)),
		identity: strings.TrimSpace(identity),
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
}

type slot struct {
	snap    *providers.AccountUsage
	started time.Time
}

type EntryRequest struct {
	Provider   string
	IdentityID string
	BaseURL    string
	APIKey     auth.Secret
}

func (t *Tracker) remember(provider string, snap *providers.AccountUsage, identity, baseURL string, started time.Time) {
	if provider == "" || snap == nil || len(snap.Windows) == 0 {
		return
	}
	var targets []slotKey
	if identity == "" {
		targets = append(targets, newSlotKey(provider, "", baseURL))
		if snap.Identity != "" {
			targets = append(targets, newSlotKey(provider, snap.Identity, ""))
		}
	} else {
		owner := snap.Identity
		if owner == "" {
			owner = identity
		}
		targets = append(targets, newSlotKey(provider, owner, ""))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, key := range targets {
		if prior, ok := t.slots[key]; ok && prior.started.After(started) {
			continue
		}
		t.slots[key] = slot{snap: snap, started: started}
	}
}

func (t *Tracker) CachedAccountUsage(provider, identityID, baseURL string) *providers.AccountUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.slots[newSlotKey(provider, identityID, baseURL)].snap
}

func SnapshotIsStale(snap *providers.AccountUsage) bool {
	if snap == nil || snap.FetchedAt.IsZero() {
		return true
	}
	age := time.Since(snap.FetchedAt)
	return age < 0 || age > staleAfter
}

func (t *Tracker) HasAccountUsage(ctx context.Context, provider string) bool {
	slug := strings.ToLower(strings.TrimSpace(provider))
	if builtinUsage(slug) {
		return true
	}
	if slug == "" {
		return false
	}
	profile, ok := t.registry.Get(t.bind(ctx), slug)
	if !ok || profile == nil {
		return false
	}
	_, fetches := profile.(providers.UsageFetcher)
	return fetches
}

func IdentityIDFor(provider string, entry *auth.Entry) string {
	key := entry.RuntimeAPIKey()
	token := strings.TrimSpace(key.Reveal())
	if token == "" {
		return "entry:" + entry.ID
	}
	if provider == auth.ProviderCodex {
		if identity := codexIdentity(token); identity != "" {
			return identity
		}
	}
	sum := sha256.Sum256([]byte(token))
	return "fp:" + hex.EncodeToString(sum[:])[:16]
}

func (t *Tracker) claimRefresh(key slotKey, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inflight[key] {
		return false
	}
	if last, ok := t.lastTry[key]; ok && now.Sub(last) < refreshAfter {
		return false
	}
	t.inflight[key] = true
	t.lastTry[key] = now
	return true
}

func (t *Tracker) releaseRefresh(key slotKey) {
	t.mu.Lock()
	delete(t.inflight, key)
	t.mu.Unlock()
}

func (t *Tracker) RefreshAccountUsageAsync(ctx context.Context, providerNames []string) {
	now := time.Now()
	seen := map[string]bool{}
	for _, provider := range providerNames {
		if seen[provider] {
			continue
		}
		seen[provider] = true
		key := newSlotKey(provider, "", "")
		if !t.claimRefresh(key, now) {
			continue
		}
		name := provider
		if !t.spawn(ctx, func(bg context.Context) {
			defer t.releaseRefresh(key)
			_, _ = t.FetchAccountUsage(bg, UsageQuery{Provider: name})
		}) {
			t.releaseRefresh(key)
		}
	}
}

func (t *Tracker) RefreshAccountUsageEntriesAsync(ctx context.Context, requests []EntryRequest) {
	now := time.Now()
	var due []EntryRequest
	for _, request := range requests {
		provider := strings.ToLower(strings.TrimSpace(request.Provider))
		identity := strings.TrimSpace(request.IdentityID)
		key := request.APIKey
		if provider == "" || identity == "" || strings.TrimSpace(key.Reveal()) == "" {
			continue
		}
		if !t.claimRefresh(newSlotKey(provider, identity, ""), now) {
			continue
		}
		due = append(due, request)
	}
	if len(due) == 0 {
		return
	}
	if !t.spawn(ctx, func(bg context.Context) {
		for _, request := range due {
			_, _ = t.FetchAccountUsage(bg, UsageQuery{
				Provider: request.Provider, BaseURL: request.BaseURL, APIKey: request.APIKey, ReadOnly: true, IdentityID: request.IdentityID,
			})
			t.releaseRefresh(newSlotKey(request.Provider, request.IdentityID, ""))
		}
	}) {
		for _, request := range due {
			t.releaseRefresh(newSlotKey(request.Provider, request.IdentityID, ""))
		}
	}
}
