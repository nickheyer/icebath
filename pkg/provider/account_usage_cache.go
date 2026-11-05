package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const (
	// AccountUsageRefreshAfter is the minimum gap between refresh tries of one usage slot
	AccountUsageRefreshAfter = 120 * time.Second
	// AccountUsageSnapshotStaleAfter is the age past which a snapshot no longer renders as a live gauge
	AccountUsageSnapshotStaleAfter = 15 * time.Minute
)

// Monotonic clock of the usage cache, replaced in tests
var accountUsageMonotonic = time.Now

// accountUsageKey is one usage slot, per profile home, provider, account identity and base URL
type accountUsageKey struct {
	home, provider, identity, baseURL string
}

var accountUsageCache = struct {
	sync.Mutex
	snapshots map[accountUsageKey]*AccountUsageSnapshot
	started   map[accountUsageKey]time.Time
	lastTry   map[accountUsageKey]time.Time
	inflight  map[accountUsageKey]bool
}{
	snapshots: map[accountUsageKey]*AccountUsageSnapshot{},
	started:   map[accountUsageKey]time.Time{},
	lastTry:   map[accountUsageKey]time.Time{},
	inflight:  map[accountUsageKey]bool{},
}

func accountUsageSlot(ctx context.Context, provider, identityID, baseURL string) accountUsageKey {
	if ctx == nil {
		ctx = context.Background()
	}
	return accountUsageKey{
		home:     home.Key(home.Dir(ctx)),
		provider: strings.ToLower(strings.TrimSpace(provider)),
		identity: strings.TrimSpace(identityID),
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
}

// HasAccountUsage reports a provider that can report usage windows through a built in fetcher or a profile hook
func HasAccountUsage(ctx context.Context, provider string) bool {
	slug := strings.ToLower(strings.TrimSpace(provider))
	if _, ok := accountUsageFetchers[slug]; ok {
		return true
	}
	if slug == "" {
		return false
	}
	profile, ok := accountUsageProfileFn(ctx, slug)
	return ok && profile.FetchAccountUsageHook != nil
}

// AccountUsageIdentityID is the stable non secret account id of a pool entry for slot keys and wire account ids
func AccountUsageIdentityID(provider string, entry PooledCredential) string {
	token := strings.TrimSpace(entry.RuntimeAPIKey())
	if token == "" {
		return "entry:" + entry.ID
	}
	if provider == "openai-codex" {
		if id := codexSnapshotIdentity(token); id != "" {
			return id
		}
	}
	sum := sha256.Sum256([]byte(token))
	return "fp:" + hex.EncodeToString(sum[:])[:16]
}

// RememberAccountUsage lands one fetch result, never erasing with an empty fetch or replacing a fresher one
func RememberAccountUsage(ctx context.Context, provider string, snapshot *AccountUsageSnapshot, identityID, baseURL string, started time.Time) {
	if provider == "" || snapshot == nil || len(snapshot.Windows) == 0 {
		return
	}
	if started.IsZero() {
		started = accountUsageMonotonic()
	}
	var targets []accountUsageKey
	if identityID == "" {
		targets = append(targets, accountUsageSlot(ctx, provider, "", baseURL))
		if snapshot.Identity != "" {
			targets = append(targets, accountUsageSlot(ctx, provider, snapshot.Identity, ""))
		}
	} else {
		targets = append(targets, accountUsageSlot(ctx, provider, usageFirstNonEmpty(snapshot.Identity, identityID), ""))
	}
	accountUsageCache.Lock()
	defer accountUsageCache.Unlock()
	for _, key := range targets {
		if prev, ok := accountUsageCache.started[key]; ok && prev.After(started) {
			continue
		}
		accountUsageCache.snapshots[key] = snapshot
		accountUsageCache.started[key] = started
	}
}

// CachedAccountUsage is the remembered snapshot of one usage slot, nil when none landed
func CachedAccountUsage(ctx context.Context, provider, identityID, baseURL string) *AccountUsageSnapshot {
	key := accountUsageSlot(ctx, provider, identityID, baseURL)
	accountUsageCache.Lock()
	defer accountUsageCache.Unlock()
	return accountUsageCache.snapshots[key]
}

// AccountUsageSnapshotIsStale reports a snapshot too old or from the future to render as a live gauge
func AccountUsageSnapshotIsStale(snapshot *AccountUsageSnapshot) bool {
	if snapshot == nil || snapshot.FetchedAt.IsZero() {
		return true
	}
	age := accountUsageNow().Sub(snapshot.FetchedAt)
	return age < 0 || age > AccountUsageSnapshotStaleAfter
}

// Takes the refresh right for a slot unless one is in flight or was tried too recently, failures included
func claimAccountUsageRefresh(key accountUsageKey, now time.Time) bool {
	accountUsageCache.Lock()
	defer accountUsageCache.Unlock()
	if accountUsageCache.inflight[key] {
		return false
	}
	if last, ok := accountUsageCache.lastTry[key]; ok && now.Sub(last) < AccountUsageRefreshAfter {
		return false
	}
	accountUsageCache.inflight[key] = true
	accountUsageCache.lastTry[key] = now
	return true
}

func releaseAccountUsageRefresh(key accountUsageKey) {
	accountUsageCache.Lock()
	defer accountUsageCache.Unlock()
	delete(accountUsageCache.inflight, key)
}

// RefreshAccountUsageAsync background refreshes the provider wide slot of each provider not tried recently
func RefreshAccountUsageAsync(ctx context.Context, providers []string) []<-chan struct{} {
	if ctx == nil {
		ctx = context.Background()
	}
	detached := context.WithoutCancel(ctx)
	var started []<-chan struct{}
	now := accountUsageMonotonic()
	seen := map[string]bool{}
	for _, provider := range providers {
		if seen[provider] {
			continue
		}
		seen[provider] = true
		key := accountUsageSlot(ctx, provider, "", "")
		if !claimAccountUsageRefresh(key, now) {
			continue
		}
		done := make(chan struct{})
		go func(provider string) {
			defer close(done)
			defer releaseAccountUsageRefresh(key)
			FetchAccountUsage(detached, provider, AccountUsageFetchOptions{})
		}(provider)
		started = append(started, done)
	}
	return started
}

// AccountUsageRefreshRequest is one live credential whose usage slot a background refresh reads
type AccountUsageRefreshRequest struct {
	Provider   string
	IdentityID string
	BaseURL    string
	APIKey     string
}

// RefreshAccountUsageEntriesAsync refreshes due per credential slots serially on one worker, read only
func RefreshAccountUsageEntriesAsync(ctx context.Context, requests []AccountUsageRefreshRequest) []<-chan struct{} {
	if ctx == nil {
		ctx = context.Background()
	}
	var due []AccountUsageRefreshRequest
	now := accountUsageMonotonic()
	for _, r := range requests {
		r.Provider = strings.ToLower(strings.TrimSpace(r.Provider))
		r.IdentityID = strings.TrimSpace(r.IdentityID)
		if r.Provider == "" || r.IdentityID == "" || strings.TrimSpace(r.APIKey) == "" {
			continue
		}
		if !claimAccountUsageRefresh(accountUsageSlot(ctx, r.Provider, r.IdentityID, ""), now) {
			continue
		}
		due = append(due, r)
	}
	if len(due) == 0 {
		return nil
	}
	detached := context.WithoutCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, r := range due {
			func() {
				defer releaseAccountUsageRefresh(accountUsageSlot(detached, r.Provider, r.IdentityID, ""))
				FetchAccountUsage(detached, r.Provider, AccountUsageFetchOptions{BaseURL: r.BaseURL, APIKey: r.APIKey,
					ReadOnly: true, IdentityID: r.IdentityID})
			}()
		}
	}()
	return []<-chan struct{}{done}
}

// Provider usage support as the model options inventory reads it
func hasAccountUsage(ctx context.Context, provider string) bool {
	return HasAccountUsage(ctx, provider)
}

// Remembered slot of the process home for inventory readers without a request context
func cachedAccountUsage(provider, identityID, baseURL string) *AccountUsageSnapshot {
	return CachedAccountUsage(context.Background(), provider, identityID, baseURL)
}

func snapshotIsStale(snapshot *AccountUsageSnapshot) bool {
	return AccountUsageSnapshotIsStale(snapshot)
}

// Background provider wide refresh whose workers the inventory never joins
func refreshAccountUsageAsync(ctx context.Context, providers []string) {
	RefreshAccountUsageAsync(ctx, providers)
}

// Background per credential refresh from provider, identity_id, base_url and api_key request dicts
func refreshAccountUsageEntriesAsync(ctx context.Context, requests []map[string]any) {
	out := make([]AccountUsageRefreshRequest, 0, len(requests))
	for _, r := range requests {
		out = append(out, AccountUsageRefreshRequest{Provider: auStr(r["provider"]), IdentityID: auStr(r["identity_id"]),
			BaseURL: auStr(r["base_url"]), APIKey: auStr(r["api_key"])})
	}
	RefreshAccountUsageEntriesAsync(ctx, out)
}

// Pool entry account id for the inventory, codex principal then token fingerprint then entry id
func identityIDFor(provider string, entry PooledCredential) string {
	return AccountUsageIdentityID(provider, entry)
}
