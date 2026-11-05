package provider

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// A reply credits state, on a window plan or not, with the named allowances full beside an open week
func planSyncState(window bool, full ...string) *CreditsState {
	s := &CreditsState{}
	if window {
		s.DenominatorKind = "usage_window"
	}
	for _, id := range full {
		s.Gauges = append(s.Gauges, WindowGauge{ID: id, Scope: ScopeAll, Label: id, UsedBp: BPFull})
	}
	s.Gauges = append(s.Gauges, WindowGauge{ID: "week", Scope: ScopeAll, Label: "week"})
	return s
}

// A portal snapshot carrying parsed gauges as id and full pairs, no usage window when pairs is nil
func planSyncSnap(pairs [][2]any, source string) *NousPortalAccountInfo {
	info := &NousPortalAccountInfo{Source: source}
	if pairs != nil {
		window := &NousUsageWindowInfo{}
		for _, p := range pairs {
			used := 0
			if p[1].(bool) {
				used = BPFull
			}
			window.Gauges = append(window.Gauges, WindowGauge{ID: p[0].(string), Scope: ScopeAll, Label: p[0].(string), UsedBp: used})
		}
		info.UsageWindow = window
	}
	return info
}

var planSyncOnPlan = planSyncSnap([][2]any{{"day", false}, {"week", false}}, "network")

type planSyncWired struct {
	calls []string
	snap  *NousPortalAccountInfo
	fetch func(ctx context.Context) *NousPortalAccountInfo
}

// Replaces the account reads, the catalog refetch and the background start with inline recorders
func wirePlanSync(t *testing.T) *planSyncWired {
	t.Helper()
	w := &planSyncWired{snap: planSyncSnap(nil, "network")}
	w.fetch = func(context.Context) *NousPortalAccountInfo { w.calls = append(w.calls, "account"); return nil }
	peek, cached, fetch, catalog, spawn, now := planSyncPeek, planSyncCached, planSyncFetch, planSyncCatalog, planSyncSpawn, planSyncNow
	planSyncCached = func(context.Context, bool) *NousPortalAccountInfo { return w.snap }
	planSyncFetch = func(ctx context.Context, force bool) *NousPortalAccountInfo { return w.fetch(ctx) }
	planSyncCatalog = func(context.Context) { w.calls = append(w.calls, "catalog nous true") }
	planSyncSpawn = func(run func()) { run() }
	planSyncMu.Lock()
	planSyncInflight, planSyncLastAttempt = map[string]bool{}, map[string]planSyncAttempt{}
	planSyncMu.Unlock()
	t.Cleanup(func() {
		planSyncPeek, planSyncCached, planSyncFetch, planSyncCatalog, planSyncSpawn, planSyncNow = peek, cached, fetch, catalog, spawn, now
	})
	return w
}

func TestPlanSnapshotStaleness(t *testing.T) {
	cases := []struct {
		state *CreditsState
		snap  *NousPortalAccountInfo
		stale bool
	}{
		{planSyncState(true), planSyncOnPlan, false},
		{planSyncState(true), planSyncSnap(nil, "network"), true},
		{planSyncState(false), planSyncOnPlan, true},
		{planSyncState(true, "day"), planSyncOnPlan, true},
		{planSyncState(true), planSyncSnap([][2]any{{"day", true}}, "network"), true},
		{planSyncState(false), planSyncSnap(nil, "network"), false},
		{planSyncState(true), nil, false},
		{planSyncState(true), planSyncSnap(nil, "error"), false},
		{nil, planSyncOnPlan, false},
	}
	for i, tc := range cases {
		if got := PlanSnapshotIsStale(tc.state, tc.snap); got != tc.stale {
			t.Errorf("case %d: stale %v want %v", i, got, tc.stale)
		}
	}
}

func TestPlanSyncAPlanChangeRefreshesAccountAndCatalogOnce(t *testing.T) {
	w := wirePlanSync(t)
	if !SyncPlanSnapshot(planSyncState(true)) || !reflect.DeepEqual(w.calls, []string{"account", "catalog nous true"}) {
		t.Fatalf("calls %v", w.calls)
	}
	if SyncPlanSnapshot(planSyncState(true)) || len(w.calls) != 2 {
		t.Fatalf("a lagging portal is not refetched on the next reply: %v", w.calls)
	}
}

func TestPlanSyncALimitChangeRefreshesOnlyTheAccount(t *testing.T) {
	w := wirePlanSync(t)
	w.snap = planSyncOnPlan
	if !SyncPlanSnapshot(planSyncState(true, "day")) || !reflect.DeepEqual(w.calls, []string{"account"}) {
		t.Fatalf("calls %v", w.calls)
	}
}

func TestPlanSyncAMismatchTheRefreshDidNotFixRetries(t *testing.T) {
	w := wirePlanSync(t)
	w.snap = planSyncOnPlan
	w.fetch = func(context.Context) *NousPortalAccountInfo {
		w.calls = append(w.calls, "account")
		return planSyncSnap(nil, "error")
	}
	for i := 0; i < 2; i++ {
		if !SyncPlanSnapshot(planSyncState(true, "day")) {
			t.Fatalf("a failed fetch rearms at once (%d)", i)
		}
	}
	clock := time.Unix(1000, 0)
	planSyncNow = func() time.Time { return clock }
	w.fetch = func(context.Context) *NousPortalAccountInfo {
		w.calls = append(w.calls, "account")
		return planSyncOnPlan
	}
	if !SyncPlanSnapshot(planSyncState(true, "day")) {
		t.Fatal("third sync must refresh")
	}
	if SyncPlanSnapshot(planSyncState(true, "day")) {
		t.Fatal("a lagging portal is not refetched on every reply")
	}
	clock = clock.Add(planSyncRetryAfter)
	if !SyncPlanSnapshot(planSyncState(true, "day")) {
		t.Fatal("the retry interval rearms")
	}
	if !reflect.DeepEqual(w.calls, []string{"account", "account", "account", "account"}) {
		t.Fatalf("calls %v", w.calls)
	}
}

func TestPlanSyncBackInStepRearms(t *testing.T) {
	w := wirePlanSync(t)
	if !SyncPlanSnapshot(planSyncState(true)) {
		t.Fatal("first mismatch refreshes")
	}
	w.snap = planSyncOnPlan
	if SyncPlanSnapshot(planSyncState(true)) {
		t.Fatal("in step")
	}
	w.snap = planSyncSnap(nil, "network")
	if !SyncPlanSnapshot(planSyncState(true)) {
		t.Fatal("back in step rearms")
	}
}

func TestPlanSyncEachProfileRefreshesItsOwnAccountUnderItsOwnHome(t *testing.T) {
	w := wirePlanSync(t)
	var homes []string
	w.fetch = func(ctx context.Context) *NousPortalAccountInfo { homes = append(homes, home.Dir(ctx)); return nil }
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		t.Setenv("ICEBATH_HOME", filepath.Join(root, name))
		if !SyncPlanSnapshot(planSyncState(true)) {
			t.Fatalf("profile %s must refresh", name)
		}
	}
	if !reflect.DeepEqual(homes, []string{filepath.Join(root, "a"), filepath.Join(root, "b")}) {
		t.Fatalf("homes %v", homes)
	}
}

func TestPlanSyncAnOrdinaryReplyCostsNothing(t *testing.T) {
	w := wirePlanSync(t)
	w.snap = planSyncOnPlan
	for i := 0; i < 5; i++ {
		if SyncPlanSnapshot(planSyncState(true)) {
			t.Fatal("in step reply refreshed")
		}
	}
	if len(w.calls) != 0 {
		t.Fatalf("calls %v", w.calls)
	}
}

func TestPlanSyncNeverRaises(t *testing.T) {
	wirePlanSync(t)
	planSyncCached = func(context.Context, bool) *NousPortalAccountInfo { panic(strconv.ErrSyntax) }
	if SyncPlanSnapshot(planSyncState(true)) {
		t.Fatal("a failing read reports no refresh")
	}
}

func TestPlanSyncANonWindowReplyWithoutAWindowSnapshotReadsNothing(t *testing.T) {
	wirePlanSync(t)
	read := false
	planSyncCached = func(context.Context, bool) *NousPortalAccountInfo { read = true; panic(strconv.ErrSyntax) }
	planSyncPeek = func(context.Context) *NousPortalAccountInfo { return nil }
	if SyncPlanSnapshot(planSyncState(false)) {
		t.Fatal("no cache entry")
	}
	planSyncPeek = func(context.Context) *NousPortalAccountInfo { return planSyncSnap(nil, "network") }
	if SyncPlanSnapshot(planSyncState(false)) || read {
		t.Fatalf("a cache entry without a window plan reads nothing, read %v", read)
	}
}
