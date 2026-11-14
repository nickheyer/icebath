package observability

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const (
	engPublic = "anthropic/claude-sonnet-4"
	engOther  = "openai/gpt-5"
)

var (
	engDay1     = float64(time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC).Unix())
	engResource = map[string]string{"architecture": "x86_64", "icebath_version": "0.0.0", "install_method": "git", "os_family": "linux"}
)

type engagementClock struct {
	mu  sync.Mutex
	now float64
}

func (c *engagementClock) set(now float64) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// Engagement clock pinned at day one nine o'clock UTC until the test moves it
func withEngagementClock(t *testing.T) *engagementClock {
	t.Helper()
	c := &engagementClock{now: engDay1}
	saved := engagementNow
	engagementNow = func() float64 {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.now
	}
	t.Cleanup(func() { engagementNow = saved })
	return c
}

type engTurnOpts struct {
	platform, model string
	tools           int
	clock           *engagementClock
	at, took        float64
}

// One completed user turn with one model call and its todo tool calls, the clock at its start and end
func engTurn(ctx context.Context, sessionID, taskID string, o engTurnOpts) {
	if o.platform == "" {
		o.platform = "cli"
	}
	if o.model == "" {
		o.model = engPublic
	}
	base := map[string]any{"session_id": sessionID, "task_id": taskID, "provider": "openrouter", "model": o.model}
	if o.clock != nil {
		o.clock.set(o.at)
	}
	invokeHook(ctx, "pre_llm_call", base, map[string]any{"platform": o.platform})
	invokeHook(ctx, "pre_api_request", base, map[string]any{"api_request_id": taskID + "-r"})
	invokeHook(ctx, "post_api_request", base, map[string]any{"api_request_id": taskID + "-r",
		"usage": map[string]any{"prompt_tokens": 1_000, "output_tokens": 5}, "context_length": 200_000})
	for n := range o.tools {
		call := mergeEvent(base, map[string]any{"tool_call_id": fmt.Sprintf("%s-c%d", taskID, n), "tool_name": "todo", "turn_id": ""})
		invokeHook(ctx, "pre_tool_call", call)
		invokeHook(ctx, "post_tool_call", call, map[string]any{"status": "success"})
	}
	if o.clock != nil {
		o.clock.set(o.at + o.took)
	}
	FinishTaskRun(ctx, sessionID, taskID, o.platform, map[string]any{"completed": true}, nil)
}

// Joins the export workers, flushes every runtime and drains every deferred store write
func settleStores(t *testing.T, ctx context.Context, stores ...*SharedMetricsStore) {
	t.Helper()
	joinExportWorkers(t, ctx)
	flushAllRuntimes(t)
	runtimesMu.Lock()
	for _, rt := range runtimes.Values() {
		if rt.Subscriber != nil {
			stores = append(stores, rt.Subscriber.Store)
		}
	}
	runtimesMu.Unlock()
	for _, s := range stores {
		s.DrainDeferred(ctx, schemaBusyTimeoutMS)
	}
}

func engRows(t *testing.T, ctx context.Context, metric string) []string {
	t.Helper()
	var out []string
	for _, v := range storedValues(t, ctx, metric) {
		out = append(out, fmt.Sprintf("%s|%d", dimsKey(v.dims), v.value))
	}
	slices.Sort(out)
	return out
}

func TestAClosedDayReportsActiveTimeSurfacesAndPrimaryModelOnce(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	clock := withEngagementClock(t)
	engTurn(ctx, "cli-1", "t1", engTurnOpts{clock: clock, at: engDay1, took: 60})
	engTurn(ctx, "cli-1", "t2", engTurnOpts{clock: clock, at: engDay1 + 180, took: 3600})
	engTurn(ctx, "gw-1", "g1", engTurnOpts{platform: "telegram", model: engOther, clock: clock, at: engDay1 + 7200, took: 30})
	finalizeSession(ctx, "cli-1")
	finalizeSession(ctx, "gw-1")
	settleStores(t, ctx)
	if got := storedValues(t, ctx, EngagementDayMetric); len(got) != 0 {
		t.Fatalf("emitted before the day closed %v", got)
	}
	other := openStore(t, metricsRoot(ctx))
	clock.set(engDay1 + 86_400)
	if err := Record(ctx, other, engResource, "cli", nil); err != nil {
		t.Fatal(err)
	}
	engTurn(ctx, "cli-2", "t3", engTurnOpts{clock: clock, at: engDay1 + 86_400 + 5})
	settleStores(t, ctx, other)
	if got := engRows(t, ctx, EngagementSurfaceMetric); !slices.Equal(got, []string{"active_minutes_bucket=5m_to_30m,surface=cli|1", "active_minutes_bucket=lt_5m,surface=gateway|1"}) {
		t.Fatalf("surface days %v", got)
	}
	want := []string{"active_minutes_bucket=5m_to_30m,active_profile_count_bucket=1,gateway_peak_busy_chats_bucket=0,primary_model=" + engPublic + ",primary_provider=openrouter,surfaces_used_count=2|1"}
	if got := engRows(t, ctx, EngagementDayMetric); !slices.Equal(got, want) {
		t.Fatalf("day rows %v", got)
	}
	rows, err := other.CounterSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	periods := map[string]bool{}
	for _, row := range rows {
		if len(row.MetricName) >= len("icebath.engagement") && row.MetricName[:len("icebath.engagement")] == "icebath.engagement" {
			periods[row.PeriodStart] = true
		}
	}
	if len(periods) != 1 || !periods["2026-09-27"] {
		t.Fatalf("periods %v", periods)
	}
}

func TestInternalAndUnattendedTurnsAreNotEngagement(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	clock := withEngagementClock(t)
	engTurn(ctx, "api-1", "a1", engTurnOpts{platform: "api_server", model: engOther, clock: clock, at: engDay1})
	engTurn(ctx, "cron-1", "c1", engTurnOpts{platform: "cron", model: engOther, clock: clock, at: engDay1 + 300, took: 7200})
	engTurn(ctx, "cli-1", "t0", engTurnOpts{clock: clock, at: engDay1 + 86_400})
	settleStores(t, ctx)
	if got := storedValues(t, ctx, EngagementDayMetric); len(got) != 0 {
		t.Fatalf("unattended day reported %v", got)
	}
	for n := range 5 {
		engTurn(ctx, "api-1", fmt.Sprintf("a%d", n+2), engTurnOpts{platform: "api_server", model: engOther, clock: clock, at: engDay1 + 86_400 + 60*float64(n)})
	}
	engTurn(ctx, "cli-1", "t1", engTurnOpts{clock: clock, at: engDay1 + 86_400 + 400})
	clock.set(engDay1 + 2*86_400)
	engTurn(ctx, "cli-1", "t2", engTurnOpts{clock: clock, at: engDay1 + 2*86_400})
	settleStores(t, ctx)
	days := storedValues(t, ctx, EngagementDayMetric)
	if len(days) != 1 || days[0].dims["primary_model"] != engPublic || days[0].dims["surfaces_used_count"] != "1" {
		t.Fatalf("day %v", days)
	}
	surfaces := storedValues(t, ctx, EngagementSurfaceMetric)
	if len(surfaces) != 1 || surfaces[0].dims["surface"] != "cli" {
		t.Fatalf("surfaces %v", surfaces)
	}
}

func TestALateInteractionNeverReopensAClosedDay(t *testing.T) {
	midnight := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC).UnixMilli()
	apply := func(state map[string]any, now int64) (map[string]any, []RollupRow) {
		t.Helper()
		next, rows, err := Apply(state, now, engResource, "cli", nil, "", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		return next, rows
	}
	state, _ := apply(nil, midnight-3_600_000)
	state, closed := apply(state, midnight+500)
	if len(closed) != 2 || closed[0].PeriodStart != "2026-09-27" || closed[1].PeriodStart != "2026-09-27" {
		t.Fatalf("closed %v", closed)
	}
	state, late := apply(state, midnight-500)
	if len(late) != 0 || state["day"] != "2026-09-28" {
		t.Fatalf("late %v day %v", late, state["day"])
	}
	_, next := apply(state, midnight+86_400_000)
	for _, r := range next {
		if r.PeriodStart != "2026-09-28" {
			t.Fatalf("next day %v", next)
		}
	}
	if len(next) == 0 {
		t.Fatal("next day closed nothing")
	}
}

func TestCollectionOffWritesNoEngagementState(t *testing.T) {
	_, ctx := directRuntime(t)
	clock := withEngagementClock(t)
	writeProfileConfig(t, home.Dir(ctx), "{}\n")
	engTurn(ctx, "s1", "t1", engTurnOpts{clock: clock, at: engDay1})
	clock.set(engDay1 + 86_400)
	engTurn(ctx, "s1", "t2", engTurnOpts{clock: clock, at: engDay1 + 86_400})
	finalizeSession(ctx, "s1")
	if _, err := os.Stat(filepath.Join(home.Dir(ctx), "telemetry")); !os.IsNotExist(err) {
		t.Fatalf("telemetry written %v", err)
	}
}

func TestSwitchAndUndoRightAfterARotationReachTheConversation(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	engTurn(ctx, "s1", "t1", engTurnOpts{})
	engTurn(ctx, "s1", "t2", engTurnOpts{})
	RotateSegment(ctx, "s1", "s1c")
	RecordSessionFriction(ctx, "undo", "s1c", map[string]string{"provider": "openrouter", "model": engPublic}, 1)
	RecordModelSwitch(ctx, "openrouter", "openrouter", "cli", engPublic, "s1c")
	settleStores(t, ctx)
	want := []string{"model=" + engPublic + ",provider=openrouter,turns_before_switch_bucket=2_to_3|1"}
	if got := engRows(t, ctx, ModelSwitchAfterMetric); !slices.Equal(got, want) {
		t.Fatalf("switch after %v", got)
	}
	wasted := storedValues(t, ctx, WastedTokensMetric)
	if len(wasted) != 1 || wasted[0].dims["tokens_bucket"] == "unknown" {
		t.Fatalf("wasted %v", wasted)
	}
}

func TestABackgroundReviewForkOnTheSessionIDAddsNoSessionVolume(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	engTurn(ctx, "s1", "t1", engTurnOpts{})
	StartTaskRun(ctx, nil, "s1", "review", "cli", "")
	base := map[string]any{"session_id": "s1", "task_id": "review", "provider": "openrouter", "model": engPublic}
	invokeHook(ctx, "pre_api_request", base, map[string]any{"api_request_id": "rv-r"})
	invokeHook(ctx, "post_api_request", base, map[string]any{"api_request_id": "rv-r", "usage": map[string]any{"prompt_tokens": 9}})
	FinishTaskRun(ctx, "s1", "review", "cli", map[string]any{"completed": true}, nil)
	finalizeSession(ctx, "s1")
	settleStores(t, ctx)
	rows := storedValues(t, ctx, SessionMetric)
	if len(rows) != 1 || rows[0].dims["turn_count_bucket"] != "1" || rows[0].dims["model_call_count_bucket"] != "1" || rows[0].dims["message_count_bucket"] != "2" {
		t.Fatalf("session rows %v", rows)
	}
}

func TestTurnsBeforeSwitchIgnoreFailoverAndReviewForks(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	engTurn(ctx, "s1", "t1", engTurnOpts{})
	base := map[string]any{"session_id": "s1", "task_id": "t2", "provider": "openrouter"}
	invokeHook(ctx, "pre_llm_call", base, map[string]any{"model": engPublic, "platform": "cli"})
	invokeHook(ctx, "pre_api_request", base, map[string]any{"model": engPublic, "api_request_id": "t2-r"})
	invokeHook(ctx, "api_request_error", base, map[string]any{"model": engPublic, "api_request_id": "t2-r"})
	invokeHook(ctx, "pre_api_request", base, map[string]any{"model": engOther, "api_request_id": "t2-fallback"})
	invokeHook(ctx, "post_api_request", base, map[string]any{"model": engOther, "api_request_id": "t2-fallback", "usage": map[string]any{}})
	FinishTaskRun(ctx, "s1", "t2", "cli", map[string]any{"completed": true}, nil)
	StartTaskRun(ctx, nil, "s1", "review", "cli", "")
	invokeHook(ctx, "pre_api_request", map[string]any{"session_id": "s1", "task_id": "review", "provider": "openrouter", "model": engOther, "api_request_id": "rv-r"})
	FinishTaskRun(ctx, "s1", "review", "cli", map[string]any{"completed": true}, nil)
	RecordModelSwitch(ctx, "openrouter", "openrouter", "cli", engPublic, "s1")
	settleStores(t, ctx)
	want := []string{"model=" + engPublic + ",provider=openrouter,turns_before_switch_bucket=2_to_3|1"}
	if got := engRows(t, ctx, ModelSwitchAfterMetric); !slices.Equal(got, want) {
		t.Fatalf("switch after %v", got)
	}
}

// Engagement day rows of one profile store as period and dimension key
func engDayRows(t *testing.T, homeDir string) []string {
	t.Helper()
	root := filepath.Join(homeDir, "telemetry", "shared_metrics")
	if _, err := os.Stat(filepath.Join(root, "metrics.sqlite3")); err != nil {
		return nil
	}
	rows, err := openStore(t, root).CounterSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, row := range rows {
		if row.MetricName == EngagementDayMetric {
			out = append(out, row.PeriodStart+"|"+dimsKey(dimsOf(row.Dimensions)))
		}
	}
	return out
}

func TestActiveProfilesAreCountedOncePerHostDayOnTheRootProfilesRow(t *testing.T) {
	for _, rootCollects := range []bool{true, false} {
		t.Run(fmt.Sprint(rootCollects), func(t *testing.T) {
			clock := withEngagementClock(t)
			root := filepath.Join(t.TempDir(), "root")
			if err := os.MkdirAll(filepath.Join(root, "profiles"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeProfileConfig(t, root, fmt.Sprintf("telemetry:\n  shared_metrics:\n    enabled: %v\n", rootCollects))
			a, b := filepath.Join(root, "profiles", "a"), filepath.Join(root, "profiles", "b")
			for _, step := range []struct {
				home string
				at   float64
			}{{a, 0}, {b, 60}, {a, 120}, {a, 86_400}} {
				clock.set(engDay1 + step.at)
				store := openStore(t, filepath.Join(step.home, "telemetry", "shared_metrics"))
				if err := Record(context.Background(), store, engResource, "cli", nil); err != nil {
					t.Fatal(err)
				}
			}
			zero := "2026-09-27|active_minutes_bucket=lt_5m,active_profile_count_bucket=0,gateway_peak_busy_chats_bucket=0,primary_model=none,primary_provider=none,surfaces_used_count=1"
			if got := engDayRows(t, a); !slices.Equal(got, []string{zero}) {
				t.Fatalf("profile a %v", got)
			}
			if got := engDayRows(t, b); len(got) != 0 {
				t.Fatalf("profile b %v", got)
			}
			if rootCollects {
				want := "2026-09-27|active_minutes_bucket=0,active_profile_count_bucket=2,gateway_peak_busy_chats_bucket=0,primary_model=none,primary_provider=none,surfaces_used_count=0"
				if got := engDayRows(t, root); !slices.Equal(got, []string{want}) {
					t.Fatalf("root %v", got)
				}
			} else if _, err := os.Stat(filepath.Join(root, "telemetry")); !os.IsNotExist(err) {
				t.Fatalf("root telemetry written %v", err)
			}
		})
	}
}

func TestLongConversationsBucketPast251(t *testing.T) {
	for _, c := range []struct {
		count  int
		bucket string
	}{{250, "101_to_250"}, {251, "251_to_1000"}, {1001, "gte_1001"}} {
		if got := LongSizeBucket(c.count); got != c.bucket || !LongSizeBuckets.Has(got) {
			t.Fatalf("%d bucket %s", c.count, got)
		}
	}
}
