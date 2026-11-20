package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

var trimEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Seconds after the test epoch as a clock reading
func trimAt(seconds float64) time.Time {
	return trimEpoch.Add(time.Duration(seconds * float64(time.Second)))
}

// Resets the trim state and seams, the release seam reporting no primitive by default
func resetTrimState(t *testing.T) {
	t.Helper()
	saved := struct {
		now     func() time.Time
		collect func()
		release func() int
		status  func() (string, bool)
	}{trimNow, trimCollect, trimRelease, readProcStatus}
	trimMu.Lock()
	lastTrim, lastAllocTrim, trimCallCount = time.Time{}, time.Time{}, 0
	trimMu.Unlock()
	idleMu.Lock()
	idleCollectedFor, idleCollectedSet, agentClosedSinceIdleCollect = time.Time{}, false, false
	idleMu.Unlock()
	trimCollect = func() {}
	trimRelease = func() int { return 0 }
	trimNow = func() time.Time { return trimAt(100) }
	t.Cleanup(func() {
		trimNow, trimCollect, trimRelease, readProcStatus = saved.now, saved.collect, saved.release, saved.status
	})
}

// Environment whose config carries the given memory_trim section
func trimEnv(section map[string]any) *tools.Env {
	return &tools.Env{Home: os.TempDir(), Config: config.Tree{"context": map[string]any{"memory_trim": section}}}
}

func floatPtr(f float64) *float64 { return &f }

func TestTrimConfigKillSwitchOverridesForceFromConfigFile(t *testing.T) {
	resetTrimState(t)
	h := filepath.Join(t.TempDir(), "icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("context:\n  memory_trim:\n    enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := 0
	trimRelease = func() int { called++; return 1 }
	ctx := home.WithOverride(context.Background(), h)
	if TrimMemory(ctx, nil, TrimOptions{Force: true}) {
		t.Fatal("kill switch must refuse a forced trim")
	}
	if called != 0 {
		t.Fatal("release ran despite the kill switch")
	}
}

func TestTrimCollectRunsEvenWithoutPagesReleased(t *testing.T) {
	resetTrimState(t)
	collects := 0
	trimCollect = func() { collects++ }
	if TrimMemory(context.Background(), trimEnv(map[string]any{}), TrimOptions{Force: true, Reason: "test"}) {
		t.Fatal("no released pages must report false")
	}
	if collects != 1 {
		t.Fatalf("collect ran %d times", collects)
	}
}

func TestTrimCollectMemorySnapshotParsesLinuxProcStatus(t *testing.T) {
	resetTrimState(t)
	readProcStatus = func() (string, bool) { return "Name:\tpython\nVmRSS:\t1234 kB\nRssAnon:\t567 kB\n", true }
	history := 42
	snap := CollectMemorySnapshot(&history)
	if snap.RSSKiB == nil || *snap.RSSKiB != 1234 || snap.RSSAnonKiB == nil || *snap.RSSAnonKiB != 567 {
		t.Fatalf("snapshot %+v", snap)
	}
	if snap.HistoryBytes == nil || *snap.HistoryBytes != 42 || snap.ThreadCount <= 0 {
		t.Fatalf("snapshot %+v", snap)
	}
	negative := -1
	if CollectMemorySnapshot(&negative).HistoryBytes != nil {
		t.Fatal("negative history bytes must be dropped")
	}
}

func TestTrimSuccessCollectsThenTrims(t *testing.T) {
	resetTrimState(t)
	var calls []string
	trimCollect = func() { calls = append(calls, "gc") }
	trimRelease = func() int { calls = append(calls, "trim"); return 1 }
	if !TrimMemory(context.Background(), trimEnv(map[string]any{}), TrimOptions{Reason: "turn", Cooldown: floatPtr(60)}) {
		t.Fatal("successful trim must report true")
	}
	if len(calls) != 2 || calls[0] != "gc" || calls[1] != "trim" {
		t.Fatalf("calls %v", calls)
	}
	if !lastTrim.Equal(trimAt(100)) {
		t.Fatalf("last trim %v", lastTrim)
	}
}

func TestTrimAllocatorOnlySkipsCollectButStillTrims(t *testing.T) {
	resetTrimState(t)
	var calls []string
	trimCollect = func() { calls = append(calls, "gc") }
	trimRelease = func() int { calls = append(calls, "trim"); return 1 }
	if !TrimMemory(context.Background(), trimEnv(map[string]any{}), TrimOptions{Reason: "post-compression", Cooldown: floatPtr(60), AllocatorOnly: true}) {
		t.Fatal("allocator only trim must report true")
	}
	if len(calls) != 1 || calls[0] != "trim" {
		t.Fatalf("calls %v", calls)
	}
}

func TestTrimCollectsByDefault(t *testing.T) {
	resetTrimState(t)
	var calls []string
	trimCollect = func() { calls = append(calls, "gc") }
	trimRelease = func() int { calls = append(calls, "trim"); return 1 }
	TrimMemory(context.Background(), trimEnv(map[string]any{}), TrimOptions{Reason: "turn", Cooldown: floatPtr(60)})
	if len(calls) != 2 || calls[0] != "gc" {
		t.Fatalf("calls %v", calls)
	}
}

func TestTrimCooldownSuppressesRepeatedCollection(t *testing.T) {
	resetTrimState(t)
	collects, releases := 0, 0
	trimCollect = func() { collects++ }
	trimRelease = func() int { releases++; return 1 }
	lastTrim = trimAt(95)
	env := trimEnv(map[string]any{})
	if TrimMemory(context.Background(), env, TrimOptions{Cooldown: floatPtr(60)}) {
		t.Fatal("trim inside the cooldown must be suppressed")
	}
	if collects != 0 || releases != 0 {
		t.Fatal("suppressed trim must not collect or release")
	}
	if !TrimMemory(context.Background(), env, TrimOptions{Force: true, Cooldown: floatPtr(60)}) {
		t.Fatal("forced trim past the floor must run")
	}
}

func TestTrimConfigCooldownControlsRateLimit(t *testing.T) {
	resetTrimState(t)
	releases := 0
	trimRelease = func() int { releases++; return 1 }
	lastTrim = trimAt(1)
	if TrimMemory(context.Background(), trimEnv(map[string]any{"enabled": true, "cooldown_seconds": 120.0}), TrimOptions{}) {
		t.Fatal("configured cooldown must suppress the trim")
	}
	if releases != 0 {
		t.Fatal("release ran inside the configured cooldown")
	}
}

func TestTrimReleaseFailureIsFailOpenAndRateLimited(t *testing.T) {
	resetTrimState(t)
	calls := 0
	trimRelease = func() int { calls++; panic("boom") }
	env := trimEnv(map[string]any{})
	if TrimMemory(context.Background(), env, TrimOptions{Reason: "test", Cooldown: floatPtr(60)}) {
		t.Fatal("failing release must report false")
	}
	if !lastTrim.Equal(trimAt(100)) {
		t.Fatal("attempt must be stamped before the release runs")
	}
	if TrimMemory(context.Background(), env, TrimOptions{Cooldown: floatPtr(60)}) {
		t.Fatal("second attempt inside the cooldown must be suppressed")
	}
	if calls != 1 {
		t.Fatalf("release called %d times", calls)
	}
}

func TestTrimForceFloorCoalescesBurstCloses(t *testing.T) {
	resetTrimState(t)
	releases := 0
	trimRelease = func() int { releases++; return 1 }
	env := trimEnv(map[string]any{"enabled": true, "cooldown_seconds": 0.0, "log_every_n": 1, "info_log_min_delta_mb": 0.0})
	trimNow = func() time.Time { return trimAt(100) }
	if !TrimMemory(context.Background(), env, TrimOptions{Force: true, Reason: "agent close"}) || releases != 1 {
		t.Fatal("first forced close must run")
	}
	for _, s := range []float64{101, 102, 103} {
		trimNow = func() time.Time { return trimAt(s) }
		if TrimMemory(context.Background(), env, TrimOptions{Force: true, Reason: "agent close"}) {
			t.Fatalf("close at %v inside the floor must coalesce", s)
		}
	}
	if releases != 1 {
		t.Fatal("burst closes must not stack forced trims")
	}
	trimNow = func() time.Time { return trimAt(106) }
	if !TrimMemory(context.Background(), env, TrimOptions{Force: true, Reason: "agent close"}) || releases != 2 {
		t.Fatal("close past the floor must run")
	}
}

func TestTrimAllocatorOnlyDoesNotSuppressForcedCollection(t *testing.T) {
	resetTrimState(t)
	var calls []string
	trimCollect = func() { calls = append(calls, "gc") }
	trimRelease = func() int { return 1 }
	env := trimEnv(map[string]any{"enabled": true, "cooldown_seconds": 60.0})
	if !TrimMemory(context.Background(), env, TrimOptions{Reason: "housekeeping", AllocatorOnly: true}) {
		t.Fatal("housekeeping trim must run")
	}
	if !TrimMemory(context.Background(), env, TrimOptions{Force: true, Reason: "agent_cache_pressure"}) {
		t.Fatal("forced collection right after an allocator only trim must run")
	}
	if len(calls) != 1 || calls[0] != "gc" {
		t.Fatalf("calls %v", calls)
	}
}

func TestTrimSettingsCoercion(t *testing.T) {
	cases := []struct {
		section  map[string]any
		enabled  bool
		cooldown float64
		every    int
		delta    float64
	}{
		{map[string]any{}, true, 60, 1, 0},
		{map[string]any{"enabled": "false", "cooldown_seconds": true, "log_every_n": 0, "info_log_min_delta_mb": -3.0}, true, 60, 1, 0},
		{map[string]any{"enabled": false, "cooldown_seconds": "5", "log_every_n": "3", "info_log_min_delta_mb": "1.5"}, false, 5, 3, 1.5},
		{map[string]any{"cooldown_seconds": -10, "log_every_n": 2.7, "info_log_min_delta_mb": "bad"}, true, 0, 2, 0},
	}
	for i, c := range cases {
		enabled, cooldown, every, delta := trimSettings(context.Background(), trimEnv(c.section))
		if enabled != c.enabled || cooldown != c.cooldown || every != c.every || delta != c.delta {
			t.Fatalf("case %d got %v %v %v %v", i, enabled, cooldown, every, delta)
		}
	}
}

// Review idle queue quiet clock driven by a fake now
type fakeQuietClock struct {
	now        float64
	liveTurns  int
	quietSince *float64
}

func (c *fakeQuietClock) turnStarted() { c.liveTurns++ }

func (c *fakeQuietClock) turnFinished() {
	c.liveTurns--
	if c.liveTurns == 0 {
		s := c.now
		c.quietSince = &s
	}
}

func (c *fakeQuietClock) QuietPeriod() (time.Duration, time.Time, bool) {
	if c.liveTurns > 0 || c.quietSince == nil {
		return 0, time.Time{}, false
	}
	return time.Duration((c.now - *c.quietSince) * float64(time.Second)), trimAt(*c.quietSince), true
}

// Clock with collections recorded and the gateway housekeeping tick bound to it
func idleClock(t *testing.T) (*fakeQuietClock, *[]float64, func()) {
	resetTrimState(t)
	clock := &fakeQuietClock{now: 1000}
	var collects []float64
	trimCollect = func() { collects = append(collects, clock.now) }
	trimRelease = func() int { return 1 }
	trimNow = func() time.Time { return trimAt(clock.now) }
	env := trimEnv(map[string]any{"enabled": true, "cooldown_seconds": 0.0, "log_every_n": 1, "info_log_min_delta_mb": 0.0})
	tick := func() { TrimMemoryWhenIdle(context.Background(), env, "messaging gateway housekeeping", clock) }
	return clock, &collects, tick
}

func TestTrimIdleCollectsOncePerQuietPeriodAndNeverDuringATurn(t *testing.T) {
	clock, collects, tick := idleClock(t)
	idle := IdleCollectAfter.Seconds()
	clock.turnStarted()
	clock.now += idle * 3
	tick()
	if len(*collects) != 0 {
		t.Fatal("a live turn must never collect")
	}
	clock.turnFinished()
	clock.now += idle - 1
	tick()
	if len(*collects) != 0 {
		t.Fatal("quiet but not long enough must not collect")
	}
	clock.now += 2
	tick()
	tick()
	if len(*collects) != 1 {
		t.Fatalf("quiet period collected %d times", len(*collects))
	}
	NoteAgentClosed()
	tick()
	tick()
	if len(*collects) != 2 {
		t.Fatalf("agent close must re-arm one collection, got %d", len(*collects))
	}
}

func TestTrimIdleNewTurnStartsNewQuietPeriod(t *testing.T) {
	clock, collects, tick := idleClock(t)
	idle := IdleCollectAfter.Seconds()
	clock.turnStarted()
	clock.turnFinished()
	clock.now += idle
	tick()
	clock.turnStarted()
	clock.turnFinished()
	clock.now++
	tick()
	if len(*collects) != 1 {
		t.Fatalf("collections %d", len(*collects))
	}
	clock.now += idle
	tick()
	if len(*collects) != 2 {
		t.Fatalf("collections %d", len(*collects))
	}
}

func TestTrimHousekeepingTickAsksForAllocatorTrimOnly(t *testing.T) {
	clock, collects, tick := idleClock(t)
	clock.turnStarted()
	tick()
	if len(*collects) != 0 {
		t.Fatal("the housekeeping tick must not run a full collection while busy")
	}
	if !lastTrim.IsZero() || lastAllocTrim.IsZero() {
		t.Fatal("the housekeeping tick must stamp an allocator only trim")
	}
}
