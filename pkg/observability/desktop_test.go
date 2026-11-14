package observability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

const desktopToday = "2026-09-27"

// Decision mark capture with the Desktop latches cleared and the UTC day pinned
func desktopMarks(t *testing.T) *markCapture {
	t.Helper()
	m := captureMarks(t)
	mustMkdir(t, m.home)
	savedDay := desktopUTCDay
	desktopLatchMu.Lock()
	desktopDaily = map[[3]string]int{}
	desktopLatchMu.Unlock()
	t.Cleanup(func() {
		desktopUTCDay = savedDay
		desktopLatchMu.Lock()
		desktopDaily = map[[3]string]int{}
		desktopLatchMu.Unlock()
	})
	desktopUTCDay = func() string { return desktopToday }
	return m
}

// Period start, string dimensions and value of every stored row of one metric
func desktopStored(t *testing.T, metric string) [][3]any {
	t.Helper()
	store, err := NewSharedMetricsStore(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.CounterSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out [][3]any
	for _, r := range rows {
		if r.MetricName != metric {
			continue
		}
		dims := map[string]string{}
		if d, ok := asDict(r.Dimensions); ok {
			for k, v := range d {
				dims[k] = fmt.Sprint(v)
			}
		} else if d, ok := r.Dimensions.(map[string]string); ok {
			dims = d
		}
		out = append(out, [3]any{r.PeriodStart, dims, r.Value})
	}
	return out
}

func TestFeatureUseCountsEachAreaOncePerDayAndCollapsesUnknownAreas(t *testing.T) {
	desktopMarks(t)
	for _, area := range []any{"terminal_pane", "terminal_pane", "settings_config_voice", "settings_brand_new_tab", "/Users/x"} {
		RecordDesktopFeatureUse(context.Background(), area)
	}
	var got []string
	for _, r := range desktopStored(t, DesktopFeatureUseMetric) {
		got = append(got, fmt.Sprintf("%s=%d", r[1].(map[string]string)["area"], r[2]))
	}
	sort.Strings(got)
	if want := []string{"other=1", "settings_config_voice=1", "settings_other=1", "terminal_pane=1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestFrictionDetailIsCheckedAgainstItsOwnKindAndCappedPerDay(t *testing.T) {
	m := desktopMarks(t)
	ctx := context.Background()
	RecordDesktopFriction(ctx, "renderer_crash", "oom")
	RecordDesktopFriction(ctx, "renderer_crash", "network")
	RecordDesktopFriction(ctx, "error_toast", "ENOSPC: /home/alice/.icebath is full")
	RecordDesktopFriction(ctx, "made_up", "crash")
	for range FrictionDailyCap + 5 {
		RecordDesktopFriction(ctx, "slow_frame", "1s_to_5s")
	}
	var pairs [][2]string
	slow := 0
	for _, r := range m.rows {
		pair := [2]string{r.Data["kind"], r.Data["detail"]}
		pairs = append(pairs, pair)
		if pair == [2]string{"slow_frame", "1s_to_5s"} {
			slow++
		}
	}
	if want := [][2]string{{"renderer_crash", "oom"}, {"renderer_crash", "other"}, {"error_toast", "other"}}; !reflect.DeepEqual(pairs[:3], want) {
		t.Fatalf("pairs %v", pairs[:3])
	}
	if slow != FrictionDailyCap {
		t.Fatalf("slow frame rows %d", slow)
	}
	assertValid(t, m.rows)
}

func TestOnboardingIsOncePerStepEventAcrossProcesses(t *testing.T) {
	m := desktopMarks(t)
	ctx := context.Background()
	RecordDesktopOnboarding(ctx, "provider_setup", "reached")
	desktopLatchMu.Lock()
	desktopDaily = map[[3]string]int{}
	desktopLatchMu.Unlock()
	RecordDesktopOnboarding(ctx, "provider_setup", "reached")
	RecordDesktopOnboarding(ctx, "provider_setup", "abandoned")
	RecordDesktopOnboarding(ctx, "not_a_step", "reached")
	var got [][2]string
	for _, r := range m.rows {
		got = append(got, [2]string{r.Data["step"], r.Data["event"]})
	}
	if want := [][2]string{{"provider_setup", "reached"}, {"provider_setup", "abandoned"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	assertValid(t, m.rows)
}

func TestTurningCollectionOffDropsTheOnboardingLatches(t *testing.T) {
	m := desktopMarks(t)
	RecordDesktopOnboarding(context.Background(), "guide", "reached")
	latches := filepath.Join(m.home, "telemetry", "shared_metrics", OnboardingLatchDirname)
	if !exists(filepath.Join(latches, "guide.reached")) {
		t.Fatal("latch missing")
	}
	PurgeOnboardingLatches(context.Background())
	if exists(latches) {
		t.Fatal("latches survived")
	}
}

func TestDisabledProfileRecordsNothingAndLeavesNoLatch(t *testing.T) {
	m := desktopMarks(t)
	ctx := context.Background()
	m.setPolicy(false)
	RecordDesktopFeatureUse(ctx, "projects")
	RecordDesktopFriction(ctx, "renderer_crash", "crash")
	RecordDesktopOnboarding(ctx, "consent", "completed")
	RecordDesktopDislike(ctx, "quick_close", "terminal_pane", nil)
	if RecordDesktopDaily(ctx, "2026-09-26", []any{map[string]any{"mode": "sessions", "active_ms": 60_000, "messages_sent": 3}}, nil, nil) {
		t.Fatal("a disabled profile settled the daily report")
	}
	if len(m.rows) != 0 || len(desktopDaily) != 0 || exists(filepath.Join(m.home, "telemetry")) {
		t.Fatalf("rows %v daily %v", m.rows, desktopDaily)
	}
	m.setPolicy(true)
	RecordDesktopFeatureUse(ctx, "projects")
	stored := desktopStored(t, DesktopFeatureUseMetric)
	if len(stored) != 1 || stored[0][1].(map[string]string)["area"] != "projects" {
		t.Fatalf("stored %v", stored)
	}
}

func TestDailyReportRecordsUsedModesAndBucketedActionsOncePerDay(t *testing.T) {
	desktopMarks(t)
	ctx := context.Background()
	modes := []any{
		map[string]any{"mode": "sessions", "active_ms": 45 * 60_000, "messages_sent": 12},
		map[string]any{"mode": "bots", "active_ms": 0, "messages_sent": 0},
	}
	actions := []any{
		map[string]any{"action": "view.toggleSidebar", "via": "shortcut", "count": 30},
		map[string]any{"action": "view.toggleSidebar", "via": "click", "count": 2},
		map[string]any{"action": "plugin.someContributedThing", "via": "palette", "count": 1},
	}
	for range 2 {
		if !RecordDesktopDaily(ctx, "2026-09-26", modes, actions, 4) {
			t.Fatal("daily report not settled")
		}
	}
	wantModes := [][3]any{{"2026-09-26", map[string]string{"active_minutes_bucket": "30m_to_2h", "bot_count_bucket": "3_to_5",
		"messages_sent_bucket": "11_to_25", "mode": "sessions"}, int64(1)}}
	if got := desktopStored(t, DesktopModeUseMetric); !reflect.DeepEqual(got, wantModes) {
		t.Fatalf("modes %v", got)
	}
	wantActions := [][3]any{
		{"2026-09-26", map[string]string{"action": "other", "count_bucket": "1", "via": "palette"}, int64(1)},
		{"2026-09-26", map[string]string{"action": "view.toggleSidebar", "count_bucket": "2", "via": "click"}, int64(1)},
		{"2026-09-26", map[string]string{"action": "view.toggleSidebar", "count_bucket": "26_to_100", "via": "shortcut"}, int64(1)},
	}
	got := desktopStored(t, DesktopActionUseMetric)
	sort.Slice(got, func(i, j int) bool { return fmt.Sprint(got[i][1]) < fmt.Sprint(got[j][1]) })
	if !reflect.DeepEqual(got, wantActions) {
		t.Fatalf("actions %v", got)
	}
}

func TestADayAndAnAreaCountOnceAcrossWindowsAndBackendRestartsInTheUsageDaysPeriod(t *testing.T) {
	desktopMarks(t)
	ctx := context.Background()
	for range 2 {
		desktopLatchMu.Lock()
		desktopDaily = map[[3]string]int{}
		desktopLatchMu.Unlock()
		if !RecordDesktopDaily(ctx, "2026-09-20", []any{map[string]any{"mode": "sessions", "active_ms": 60_000, "messages_sent": 3}},
			[]any{map[string]any{"action": "composer.send", "count": 4, "via": "shortcut"}}, nil) {
			t.Fatal("daily report not settled")
		}
		RecordDesktopFeatureUse(ctx, "terminal_pane")
	}
	for _, metric := range []string{DesktopActionUseMetric, DesktopModeUseMetric} {
		got := desktopStored(t, metric)
		if len(got) != 1 || got[0][0] != "2026-09-20" || got[0][2] != int64(1) {
			t.Fatalf("%s %v", metric, got)
		}
	}
	if got := desktopStored(t, DesktopFeatureUseMetric); len(got) != 1 || got[0][2] != int64(1) {
		t.Fatalf("feature use %v", got)
	}
}

func TestDailyReportThatTheStoreRefusesStaysWithTheClient(t *testing.T) {
	desktopMarks(t)
	ctx := context.Background()
	modes := []any{map[string]any{"mode": "bots", "active_ms": 1, "messages_sent": 0}}
	savedRollup := updateRollupStateFn
	updateRollupStateFn = func(*SharedMetricsStore, context.Context, string, func(map[string]any) (any, []RollupRow)) error {
		return errors.New("database is locked")
	}
	if RecordDesktopDaily(ctx, "2026-09-26", modes, nil, nil) {
		t.Fatal("a refused report was settled")
	}
	updateRollupStateFn = savedRollup
	if !RecordDesktopDaily(ctx, "2026-09-26", modes, nil, nil) {
		t.Fatal("daily report not settled")
	}
	got := desktopStored(t, DesktopModeUseMetric)
	if len(got) != 1 || got[0][1].(map[string]string)["active_minutes_bucket"] != "lt_5m" {
		t.Fatalf("modes %v", got)
	}
}

func TestDislikeTargetsAreClosedPerSignal(t *testing.T) {
	m := desktopMarks(t)
	ctx := context.Background()
	for _, p := range [][2]string{{"quick_close", "browser_pane"}, {"cancelled", "model_picker"}, {"rage_click", "composer.send"},
		{"undo", "restored_draft"}, {"feature_disabled", "tips"}, {"quick_close", "keybind_capture"}, {"loathing", "tips"}} {
		RecordDesktopDislike(ctx, p[0], p[1], nil)
	}
	var got [][2]string
	for _, r := range m.rows {
		got = append(got, [2]string{r.Data["signal"], r.Data["target"]})
		if r.Data["setting"] != "none" || r.Data["direction"] != "none" {
			t.Fatalf("setting fields %v", r.Data)
		}
	}
	want := [][2]string{{"quick_close", "browser_pane"}, {"cancelled", "model_picker"}, {"rage_click", "composer.send"},
		{"undo", "restored_draft"}, {"feature_disabled", "tips"}, {"quick_close", "other"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	assertValid(t, m.rows)
}

func TestSettingChangeSendsOnlyTheSchemaKeyAndTheBackendDecidesTheDirection(t *testing.T) {
	m := desktopMarks(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(m.home, "config.yaml"), []byte("display:\n  compact: true\n  skin: default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"display.compact", "display.skin", "providers.my-secret-box.api_key"} {
		RecordDesktopDislike(ctx, "setting_off_default", nil, key)
	}
	var got [][3]string
	for _, r := range m.rows {
		got = append(got, [3]string{r.Data["target"], r.Data["setting"], r.Data["direction"]})
	}
	want := [][3]string{{"setting", "display.compact", "away_from_default"}, {"setting", "display.skin", "to_default"},
		{"setting", "other", "none"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	assertValid(t, m.rows)
}
