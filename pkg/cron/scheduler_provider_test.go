package cron

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

type testCronScheduler struct {
	registerErr error
	registered  []string
	changed     int
}

func (t *testCronScheduler) Name() string                                         { return "builtin" }
func (t *testCronScheduler) Start(context.Context, <-chan struct{}, StartOptions) {}
func (t *testCronScheduler) RegisterJob(job *Job) error {
	t.registered = append(t.registered, job.ID)
	return t.registerErr
}
func (t *testCronScheduler) OnJobsChanged() error { t.changed++; return nil }

var testScheduler = &testCronScheduler{}

func init() { schedulerResolver = func(*Store) CronScheduler { return testScheduler } }

type minimalProvider struct{ name string }

func (m *minimalProvider) Name() string                                         { return m.name }
func (m *minimalProvider) Start(context.Context, <-chan struct{}, StartOptions) {}

type legacyProvider struct{ minimalProvider }

func (l *legacyProvider) FireDue(context.Context, string, FireOptions) (bool, error) {
	return true, nil
}
func (l *legacyProvider) FireDueKeywords() ([]string, bool) {
	return []string{"adapters", "loop"}, false
}

type sinkProvider struct{ legacyProvider }

func (k *sinkProvider) FireDueKeywords() ([]string, bool) { return nil, true }

type splitProvider struct{ legacyProvider }

func (p *splitProvider) ClaimFire(context.Context, string, bool, bool) (*Job, error) { return nil, nil }

type unavailableProvider struct{ minimalProvider }

func (u *unavailableProvider) IsAvailable() bool { return false }

func providerConfig(t *testing.T, s *Store, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackoffWaitSecondsDoublesAndCaps(t *testing.T) {
	for _, c := range []struct {
		failures int
		want     float64
	}{{0, 60}, {-1, 60}, {1, 60}, {2, 120}, {4, 480}, {6, 900}, {40, 900}} {
		if got := backoffWaitSeconds(60, c.failures); got != c.want {
			t.Fatal(c.failures, got)
		}
	}
}

func TestForceFireCapabilityDetectsLegacyOverride(t *testing.T) {
	if !providerSupportsForceFire(&minimalProvider{"current"}) || providerSupportsForceFire(&legacyProvider{}) || !providerSupportsForceFire(&sinkProvider{}) {
		t.Fatal("force fire detection")
	}
	if !providerSupportsSplitFire(&minimalProvider{}) || providerSupportsSplitFire(&legacyProvider{}) || !providerSupportsSplitFire(&splitProvider{}) {
		t.Fatal("split fire detection")
	}
	if schedulerOnJobsChanged(&minimalProvider{}) != nil || schedulerRegisterJob(&minimalProvider{}, &Job{}) != nil || schedulerReconcile(&minimalProvider{}) != nil || !schedulerIsAvailable(&minimalProvider{}) {
		t.Fatal("additive hooks must default safely")
	}
}

func TestResolveCronSchedulerFallsBackToBuiltin(t *testing.T) {
	s, logs := newTestStore(t)
	t.Cleanup(func() { testRun.providers, testRun.loadErr = map[string]CronScheduler{}, nil })
	if _, ok := resolveCronScheduler(s).(*InProcessCronScheduler); !ok {
		t.Fatal("no config must resolve the builtin")
	}
	providerConfig(t, s, "cron:\n  provider: 7\n")
	if _, ok := resolveCronScheduler(s).(*InProcessCronScheduler); !ok {
		t.Fatal("a non string provider must resolve the builtin")
	}
	providerConfig(t, s, "cron:\n  provider: nonexistent\n")
	if _, ok := resolveCronScheduler(s).(*InProcessCronScheduler); !ok || !strings.Contains(logs.String(), "cron.provider 'nonexistent' not found; using built-in ticker") {
		t.Fatal(logs.String())
	}
	testRun.providers["dead"] = &unavailableProvider{minimalProvider{"dead"}}
	providerConfig(t, s, "cron:\n  provider: dead\n")
	if _, ok := resolveCronScheduler(s).(*InProcessCronScheduler); !ok || !strings.Contains(logs.String(), "cron.provider 'dead' not available") {
		t.Fatal(logs.String())
	}
	live := &minimalProvider{"live"}
	testRun.providers["live"] = live
	providerConfig(t, s, "cron:\n  provider: ' live '\n")
	if got := resolveCronScheduler(s); got != live {
		t.Fatal(got)
	}
	if got := s.schedulerForProfileMode(live, true); got.Name() != "builtin" || !strings.Contains(logs.String(), "does not support multiplex_profiles") {
		t.Fatal(got)
	}
	if got := s.schedulerForProfileMode(live, false); got != live {
		t.Fatal(got)
	}
	testRun.loadErr = errors.New("boom")
	if _, ok := resolveCronScheduler(s).(*InProcessCronScheduler); !ok || !strings.Contains(logs.String(), "Failed to load cron.provider 'live' (boom); using built-in ticker") {
		t.Fatal(logs.String())
	}
}

func TestFireDueDefaultClaimsThenRunsAndLostClaimDoesNot(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := withCronStore(t.Context(), s)
	job, _ := s.createJob(CreateJobOptions{Prompt: prompt("x"), Schedule: "every 1h"})
	resetRan()
	p := &minimalProvider{"chronos"}
	ok, err := schedulerFireDue(ctx, p, job.ID, FireOptions{Force: true})
	if err != nil || !ok || len(ranJobs()) != 1 || ranJobs()[0] != job.ID {
		t.Fatal(ok, err, ranJobs())
	}
	rows, _ := openExecutionsLedger(s).List(ctx, &job.ID, 10, nil)
	if len(rows) != 1 || rows[0]["source"] != "chronos" {
		t.Fatal(rows)
	}
	resetRan()
	if ok, err := schedulerFireDue(ctx, p, "missing", FireOptions{}); ok || err != nil || len(ranJobs()) != 0 {
		t.Fatal(ok, err)
	}
	missing := "missing"
	rows, _ = openExecutionsLedger(s).List(ctx, &missing, 10, nil)
	if len(rows) != 1 || rows[0]["status"] == "running" || !strings.Contains(rows[0]["error"].(string), "Fire claim was not acquired") {
		t.Fatal(rows)
	}
}

func startTicker(t *testing.T, s *Store, o StartOptions) (stop func()) {
	t.Helper()
	ch := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		(&InProcessCronScheduler{}).Start(withCronStore(context.Background(), s), ch, o)
	}()
	var once sync.Once
	return func() { once.Do(func() { close(ch); wg.Wait() }) }
}

func waitUntil(cond func() bool) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return false
}

func TestInprocessProviderTicksStopsAndFailingTickRecordsLivenessOnly(t *testing.T) {
	s, _ := newTestStore(t)
	var mu sync.Mutex
	calls := 0
	tickHook = func(context.Context, *Store, tickOptions) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return 0, errors.New("every tick fails")
	}
	t.Cleanup(func() { tickHook = schedulerTick })
	zero := 0.0
	stop := startTicker(t, s, StartOptions{Interval: &zero})
	if !waitUntil(func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 2 }) {
		t.Fatal("ticker did not tick")
	}
	stop()
	if _, ok := s.TickerHeartbeatAge(); !ok {
		t.Fatal("liveness heartbeat missing")
	}
	if _, ok := s.TickerSuccessAge(); ok {
		t.Fatal("a failing tick bumped the success marker")
	}
	if msg, _ := s.TickerLastError(); !strings.Contains(msg, "every tick fails") {
		t.Fatal(msg)
	}
	tickHook = func(context.Context, *Store, tickOptions) (int, error) { return 0, nil }
	stop = startTicker(t, s, StartOptions{Interval: &zero})
	if !waitUntil(func() bool { _, ok := s.TickerSuccessAge(); return ok }) {
		t.Fatal("success never recorded")
	}
	stop()
	if _, ok := s.TickerLastError(); ok {
		t.Fatal("a good tick must clear the error marker")
	}
}

func TestExistingProfileHomesFiltersDeleted(t *testing.T) {
	s, logs := newTestStore(t)
	live, gone := t.TempDir(), filepath.Join(t.TempDir(), "gone")
	name := "work"
	got := s.existingProfileHomes([]ProfileHome{{Name: &name, Home: live}, {Home: gone}}, nil)
	if len(got) != 1 || got[0].Home != live {
		t.Fatal(got)
	}
	if got := s.existingProfileHomes(nil, func() ([]ProfileHome, error) { return nil, errors.New("enum") }); len(got) != 0 || !strings.Contains(logs.String(), "cron profile enumeration failed") {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(gone, "cron")); err == nil {
		t.Fatal("a deleted profile was recreated")
	}
}

func TestMultiplexTickerTicksEachProfileAndIsolatesFailures(t *testing.T) {
	s, _ := newTestStore(t)
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	for _, h := range []string{a, b} {
		_ = os.MkdirAll(filepath.Join(h, "cron"), 0o700)
	}
	var mu sync.Mutex
	ticked := map[string]int{}
	tickHook = func(_ context.Context, ps *Store, _ tickOptions) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		ticked[ps.home]++
		if strings.HasSuffix(ps.home, "/b") {
			return 0, errors.New("profile b broke")
		}
		return 0, nil
	}
	t.Cleanup(func() { tickHook = schedulerTick })
	zero := 0.0
	na, nb := "a", "b"
	stop := startTicker(t, s, StartOptions{Interval: &zero, ProfileHomes: []ProfileHome{{Name: &na, Home: a}, {Name: &nb, Home: b}}})
	if !waitUntil(func() bool { mu.Lock(); defer mu.Unlock(); return len(ticked) == 2 }) {
		t.Fatal(ticked)
	}
	stop()
	sa, sb := openCronStore(a, nil), openCronStore(b, nil)
	if _, ok := sa.TickerSuccessAge(); !ok {
		t.Fatal("healthy profile must record success")
	}
	if _, ok := sb.TickerSuccessAge(); ok {
		t.Fatal("failing profile recorded success")
	}
	if msg, _ := sb.TickerLastError(); !strings.Contains(msg, "profile b broke") {
		t.Fatal(msg)
	}
}

func TestFireOverdueJobsBackstop(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	ctx := withCronStore(t.Context(), s)
	job, _ := s.createJob(CreateJobOptions{Prompt: prompt("x"), Schedule: "every 1h"})
	past := FormatISO(now.Add(-30 * time.Minute))
	if _, err := s.updateJob(job.ID, map[string]any{"next_run_at": past}); err != nil {
		t.Fatal(err)
	}
	if n := fireOverdueJobs(ctx, &InProcessCronScheduler{}, FireOptions{}, nil); n != 0 {
		t.Fatal("builtin is a no op", n)
	}
	saved := estopCheckPaused
	RegisterEstopCheck(func(string, *slog.Logger) bool { return true })
	if n := fireOverdueJobs(ctx, &minimalProvider{"ext"}, FireOptions{}, nil); n != 0 {
		t.Fatal("estop must hold the backstop")
	}
	estopCheckPaused = saved
	providerConfig(t, s, "cron:\n  misfire_grace_minutes: 0\n")
	if n := fireOverdueJobs(ctx, &minimalProvider{"ext"}, FireOptions{}, nil); n != 0 {
		t.Fatal("non positive grace disables the sweep")
	}
	providerConfig(t, s, "cron:\n  misfire_grace_minutes: 10\n")
	resetRan()
	if n := fireOverdueJobs(ctx, &minimalProvider{"ext"}, FireOptions{}, nil); n != 1 {
		t.Fatal(n)
	}
	if !waitUntil(func() bool { return len(ranJobs()) == 1 }) {
		t.Fatal(ranJobs())
	}
}

func TestRoutedProfileFireComparesTheLaunchHome(t *testing.T) {
	launch := home.RoutingProcess()
	if routedProfileFire(t.Context(), &launch) {
		t.Fatal("the launch home is not a routed fire")
	}
	other := t.TempDir()
	if !routedProfileFire(t.Context(), &other) {
		t.Fatal("a sibling home is a routed fire")
	}
}
