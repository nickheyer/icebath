package provider

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const planSyncRetryAfter = 120 * time.Second

type planSyncSeen struct {
	onWindow bool
	fullIDs  string
}

type planSyncAttempt struct {
	seen planSyncSeen
	at   time.Time
}

var (
	planSyncMu          sync.Mutex
	planSyncInflight    = map[string]bool{}
	planSyncLastAttempt = map[string]planSyncAttempt{}
	planSyncNow         = time.Now
	planSyncPeek        = PeekNousAccountInfoCache
	planSyncCached      = CachedNousPortalAccountInfo
	planSyncFetch       = GetNousPortalAccountInfo
	planSyncCatalog     = func(ctx context.Context) { GetPricingForProvider(ctx, "nous", "", true, false) }
	planSyncSpawn       = func(run func()) { go run() }
)

// Sorted joined ids of the full gauges, a stable set key
func fullGaugeIDs(gauges []WindowGauge) string {
	var ids []string
	for _, g := range gauges {
		if g.Full() {
			ids = append(ids, g.ID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, "\x00")
}

// Full allowance ids of the cached snapshot, false when it has no window plan
func snapshotFullIDs(snapshot *NousPortalAccountInfo) (string, bool) {
	if snapshot == nil || !applyWindowSeedFields(snapshot.UsageWindow, nil) {
		return "", false
	}
	return fullGaugeIDs(snapshot.UsageWindow.Gauges), true
}

// PlanSnapshotIsStale reports a reply contradicting the cached plan in a way a picker shows
func PlanSnapshotIsStale(state *CreditsState, snapshot *NousPortalAccountInfo) bool {
	if state == nil || snapshot == nil || snapshot.Source == "error" {
		return false
	}
	onWindow := state.IsUsageWindow()
	fullIDs, snapWindow := snapshotFullIDs(snapshot)
	if onWindow != snapWindow {
		return true
	}
	if !onWindow {
		return false
	}
	return fullGaugeIDs(state.Gauges) != fullIDs
}

// Refetches the account and on a plan change the catalog, a failed fetch counts as no attempt
func planSyncRefresh(ctx context.Context, planChanged bool, key string) {
	defer func() {
		if r := recover(); r != nil {
			planSyncMu.Lock()
			delete(planSyncLastAttempt, key)
			planSyncMu.Unlock()
		}
		planSyncMu.Lock()
		delete(planSyncInflight, key)
		planSyncMu.Unlock()
	}()
	if info := planSyncFetch(ctx, true); info != nil && info.Source == "error" {
		planSyncMu.Lock()
		delete(planSyncLastAttempt, key)
		planSyncMu.Unlock()
	}
	if planChanged {
		planSyncCatalog(ctx)
	}
}

// SyncPlanSnapshot starts one background account refresh when a reply says the cached plan is out of date
func SyncPlanSnapshot(state *CreditsState) (started bool) {
	ctx := creditsCtx()
	key := home.Key(home.Dir(ctx))
	defer func() {
		if r := recover(); r != nil {
			planSyncMu.Lock()
			delete(planSyncInflight, key)
			planSyncMu.Unlock()
			started = false
		}
	}()
	onWindow := state != nil && state.IsUsageWindow()
	if !onWindow {
		if _, cachedWindow := snapshotFullIDs(planSyncPeek(ctx)); !cachedWindow {
			planSyncMu.Lock()
			delete(planSyncLastAttempt, key)
			planSyncMu.Unlock()
			return false
		}
	}
	snapshot := planSyncCached(ctx, true)
	if !PlanSnapshotIsStale(state, snapshot) {
		planSyncMu.Lock()
		delete(planSyncLastAttempt, key)
		planSyncMu.Unlock()
		return false
	}
	seen := planSyncSeen{onWindow: onWindow, fullIDs: fullGaugeIDs(state.Gauges)}
	_, snapWindow := snapshotFullIDs(snapshot)
	planChanged := onWindow != snapWindow
	now := planSyncNow()
	planSyncMu.Lock()
	last, hasLast := planSyncLastAttempt[key]
	if planSyncInflight[key] || (hasLast && last.seen == seen && now.Sub(last.at) < planSyncRetryAfter) {
		planSyncMu.Unlock()
		return false
	}
	planSyncInflight[key] = true
	planSyncLastAttempt[key] = planSyncAttempt{seen: seen, at: now}
	planSyncMu.Unlock()
	planSyncSpawn(func() { planSyncRefresh(ctx, planChanged, key) })
	return true
}
