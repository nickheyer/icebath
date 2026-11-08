package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestDeferModeValues(t *testing.T) {
	cases := []struct {
		cfg  map[string]any
		want string
	}{
		{nil, "auto"}, {map[string]any{}, "auto"}, {map[string]any{"defer": "auto"}, "auto"},
		{map[string]any{"defer": "never"}, "never"}, {map[string]any{"defer": "NEVER"}, "never"},
		{map[string]any{"defer": "sometimes"}, "auto"}, {map[string]any{"defer": 3}, "auto"},
	}
	for _, c := range cases {
		if got := reviewDeferMode(c.cfg); got != c.want {
			t.Fatalf("%v: %q", c.cfg, got)
		}
	}
}

func TestDeferMaxAgeParsing(t *testing.T) {
	def := reviewDeferMaxAgeS(nil)
	if def <= 0 {
		t.Fatal("default not positive")
	}
	cases := []struct {
		v    any
		want float64
	}{{120, 120}, {"600", 600}, {"soon", def}, {0, def}, {-5, def}}
	for _, c := range cases {
		if got := reviewDeferMaxAgeS(map[string]any{"defer_max_age_s": c.v}); got != c.want {
			t.Fatalf("%v: %v", c.v, got)
		}
	}
}

type fakeReviewAgent struct{ spawned []backgroundReviewKwargs }

func (f *fakeReviewAgent) spawnBackgroundReviewNow(_ context.Context, kw backgroundReviewKwargs) {
	f.spawned = append(f.spawned, kw)
}

func makeReviewQueue(serverIdle bool) (*reviewIdleQueue, *float64) {
	q := newReviewIdleQueue()
	clock := new(float64)
	q.now = func() float64 { return *clock }
	q.serverIdle = func() bool { return serverIdle }
	q.ensureThread = func() {}
	return q, clock
}

func cfgKw(cfg map[string]any) backgroundReviewKwargs { return backgroundReviewKwargs{taskCfg: cfg} }

func TestEnqueueCoalescesPerSessionNewestWinsOldestAge(t *testing.T) {
	q, clock := makeReviewQueue(true)
	agent := &fakeReviewAgent{}
	ctx := context.Background()
	*clock = 100
	q.enqueue(ctx, agent, "s1", backgroundReviewKwargs{messagesSnapshot: []map[string]any{{"x": "old"}}, taskCfg: map[string]any{}})
	*clock = 200
	q.enqueue(ctx, agent, "s1", backgroundReviewKwargs{messagesSnapshot: []map[string]any{{"x": "new"}}, taskCfg: map[string]any{}})
	q.enqueue(ctx, agent, "s2", backgroundReviewKwargs{messagesSnapshot: []map[string]any{{"x": "other"}}, taskCfg: map[string]any{}})
	if q.pendingCount() != 2 {
		t.Fatalf("pending %d", q.pendingCount())
	}
	q.mu.Lock()
	item := q.pending["s1"]
	q.mu.Unlock()
	if item.kwargs.messagesSnapshot[0]["x"] != "new" || item.enqueuedAt != 100 {
		t.Fatalf("item %v at %v", item.kwargs.messagesSnapshot, item.enqueuedAt)
	}
}

func TestDispatchWaitsForSustainedQuiet(t *testing.T) {
	q, clock := makeReviewQueue(true)
	q.enqueue(context.Background(), &fakeReviewAgent{}, "s1", cfgKw(map[string]any{}))
	q.noteTurnStarted()
	if q.popDispatchable() != nil {
		t.Fatal("dispatched during a live turn")
	}
	q.noteTurnFinished()
	if q.popDispatchable() != nil {
		t.Fatal("dispatched before the settle window")
	}
	*clock += reviewIdleSettleSeconds + 1
	item := q.popDispatchable()
	if item == nil || item.sessionKey != "s1" || q.pendingCount() != 0 {
		t.Fatalf("item %v pending %d", item, q.pendingCount())
	}
}

func TestDispatchBlockedByBusyServer(t *testing.T) {
	q, clock := makeReviewQueue(false)
	q.enqueue(context.Background(), &fakeReviewAgent{}, "s1", cfgKw(map[string]any{}))
	q.noteTurnStarted()
	q.noteTurnFinished()
	*clock += reviewIdleSettleSeconds + 1
	if q.popDispatchable() != nil || q.pendingCount() != 1 {
		t.Fatal("dispatched onto a busy server")
	}
}

func TestAgedOutItemDispatchesDespiteBusyServer(t *testing.T) {
	q, clock := makeReviewQueue(false)
	q.enqueue(context.Background(), &fakeReviewAgent{}, "s1", cfgKw(map[string]any{"defer_max_age_s": 60}))
	q.noteTurnStarted()
	*clock += 61
	if item := q.popDispatchable(); item == nil || item.sessionKey != "s1" {
		t.Fatalf("aged out item %v", item)
	}
}

func TestNewTurnResetsTheQuietClock(t *testing.T) {
	q, clock := makeReviewQueue(true)
	q.enqueue(context.Background(), &fakeReviewAgent{}, "s1", cfgKw(map[string]any{}))
	q.noteTurnStarted()
	q.noteTurnFinished()
	*clock += reviewIdleSettleSeconds - 2
	q.noteTurnStarted()
	*clock += 30
	if q.popDispatchable() != nil {
		t.Fatal("dispatched during a live turn")
	}
	q.noteTurnFinished()
	if q.popDispatchable() != nil {
		t.Fatal("settle window did not restart")
	}
	*clock += reviewIdleSettleSeconds + 1
	if q.popDispatchable() == nil {
		t.Fatal("quiet queue held the review")
	}
}

func TestNestedTurnsRequireAllToFinish(t *testing.T) {
	q, clock := makeReviewQueue(true)
	q.enqueue(context.Background(), &fakeReviewAgent{}, "s1", cfgKw(map[string]any{}))
	q.noteTurnStarted()
	q.noteTurnStarted()
	q.noteTurnFinished()
	*clock += reviewIdleSettleSeconds + 1
	if q.popDispatchable() != nil {
		t.Fatal("dispatched while one turn was live")
	}
	q.noteTurnFinished()
	*clock += reviewIdleSettleSeconds + 1
	if q.popDispatchable() == nil {
		t.Fatal("quiet queue held the review")
	}
}

type wrapperCalls struct {
	enqueued []struct {
		key string
		kw  backgroundReviewKwargs
	}
	spawned []backgroundReviewKwargs
}

// Agent wearing the real spawn wrapper with the settings, classifier, queue and fork thread recorded
func wrapperAgent(t *testing.T, deferMode string, managed bool) (*AIAgent, *wrapperCalls) {
	settings, targets, enqueue, spawn := loadBackgroundReviewSettings, reviewTargetsManagedLocal, enqueueIdleReview, spawnBackgroundReviewThread
	t.Cleanup(func() {
		loadBackgroundReviewSettings, reviewTargetsManagedLocal, enqueueIdleReview, spawnBackgroundReviewThread = settings, targets, enqueue, spawn
	})
	calls := &wrapperCalls{}
	loadBackgroundReviewSettings = func(context.Context) (bool, map[string]any) {
		return true, map[string]any{"defer": deferMode}
	}
	reviewTargetsManagedLocal = func(context.Context, *AIAgent, map[string]any) bool { return managed }
	enqueueIdleReview = func(_ context.Context, _ *AIAgent, key string, kw backgroundReviewKwargs) {
		calls.enqueued = append(calls.enqueued, struct {
			key string
			kw  backgroundReviewKwargs
		}{key, kw})
	}
	spawnBackgroundReviewThread = func(_ context.Context, a *AIAgent, kw backgroundReviewKwargs, run *backgroundReviewRun) (func(context.Context), string, error) {
		calls.spawned = append(calls.spawned, kw)
		return func(context.Context) { finishBackgroundReviewRun(a, run) }, "bg-review", nil
	}
	return &AIAgent{sessionID: "sess-x"}, calls
}

var hiSnapshot = []map[string]any{{"role": "user", "content": "hi"}}

func TestWrapperDefersManagedLocalAuto(t *testing.T) {
	a, calls := wrapperAgent(t, "auto", true)
	a.spawnBackgroundReviewWith(context.Background(), hiSnapshot, true, false, nil, false)
	if len(calls.enqueued) != 1 || len(calls.spawned) != 0 {
		t.Fatalf("enqueued %d spawned %d", len(calls.enqueued), len(calls.spawned))
	}
	if calls.enqueued[0].key != "sess-x" || !calls.enqueued[0].kw.reviewMemory {
		t.Fatalf("enqueued %+v", calls.enqueued[0])
	}
}

func TestWrapperSpawnsImmediatelyForNonManaged(t *testing.T) {
	a, calls := wrapperAgent(t, "auto", false)
	a.spawnBackgroundReviewWith(context.Background(), hiSnapshot, false, true, nil, false)
	if len(calls.enqueued) != 0 || len(calls.spawned) != 1 {
		t.Fatalf("enqueued %d spawned %d", len(calls.enqueued), len(calls.spawned))
	}
}

func TestWrapperDeferNeverIsOldBehavior(t *testing.T) {
	a, calls := wrapperAgent(t, "never", true)
	a.spawnBackgroundReviewWith(context.Background(), hiSnapshot, true, false, nil, false)
	if len(calls.enqueued) != 0 || len(calls.spawned) != 1 {
		t.Fatalf("enqueued %d spawned %d", len(calls.enqueued), len(calls.spawned))
	}
}

func TestWrapperRefineBypassesQueue(t *testing.T) {
	a, calls := wrapperAgent(t, "auto", true)
	focus := "save the deploy workflow"
	a.spawnBackgroundReviewWith(context.Background(), hiSnapshot, true, false, &focus, false)
	if len(calls.enqueued) != 0 || len(calls.spawned) != 1 || *calls.spawned[0].focus != focus {
		t.Fatalf("enqueued %d spawned %+v", len(calls.enqueued), calls.spawned)
	}
}

func TestWrapperBareRefineBypassesQueue(t *testing.T) {
	a, calls := wrapperAgent(t, "auto", true)
	a.spawnBackgroundReviewWith(context.Background(), hiSnapshot, true, false, nil, true)
	if len(calls.enqueued) != 0 || len(calls.spawned) != 1 {
		t.Fatalf("enqueued %d spawned %d", len(calls.enqueued), len(calls.spawned))
	}
}

func TestWrapperCloudFastPathSkipsRuntimeResolution(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	resolve := resolveReviewRuntime
	t.Cleanup(func() { resolveReviewRuntime = resolve })
	resolved := 0
	resolveReviewRuntime = func(context.Context, *AIAgent, map[string]any) reviewRuntime {
		resolved++
		panic("runtime resolution must not run")
	}
	if reviewTargetsManagedLocal(context.Background(), &AIAgent{}, map[string]any{}) || resolved != 0 {
		t.Fatalf("managed reported or resolved %d", resolved)
	}
}

func TestDispatcherRechecksEnabledGate(t *testing.T) {
	q, clock := makeReviewQueue(true)
	q.enqueue(context.Background(), &fakeReviewAgent{}, "s1", cfgKw(map[string]any{}))
	settings := loadBackgroundReviewSettings
	t.Cleanup(func() { loadBackgroundReviewSettings = settings })
	loadBackgroundReviewSettings = func(context.Context) (bool, map[string]any) { return false, map[string]any{} }
	*clock += reviewIdleSettleSeconds + 1
	q.noteTurnStarted()
	q.noteTurnFinished()
	*clock += reviewIdleSettleSeconds + 1
	item := q.popDispatchable()
	if item == nil || reviewStillEnabled(item) {
		t.Fatalf("item %v survived the disabled gate", item)
	}
}

// Agent wearing the real requeue step with the classifier and queue recorded
func requeueAgent(t *testing.T, managed bool) (*AIAgent, *[]backgroundReviewKwargs) {
	targets, enqueue := reviewTargetsManagedLocal, enqueueIdleReview
	t.Cleanup(func() { reviewTargetsManagedLocal, enqueueIdleReview = targets, enqueue })
	var enqueued []backgroundReviewKwargs
	reviewTargetsManagedLocal = func(context.Context, *AIAgent, map[string]any) bool { return managed }
	enqueueIdleReview = func(_ context.Context, _ *AIAgent, _ string, kw backgroundReviewKwargs) {
		enqueued = append(enqueued, kw)
	}
	return &AIAgent{sessionID: "sess-x"}, &enqueued
}

func reviewRunWith(cancelled bool) *backgroundReviewRun {
	run := &backgroundReviewRun{}
	if cancelled {
		run.cancelRequested.Set()
	}
	return run
}

func requeueKw(attempts int) backgroundReviewKwargs {
	return backgroundReviewKwargs{taskCfg: map[string]any{"defer": "auto"}, requeueAttempts: attempts}
}

func TestPreemptedReviewRequeues(t *testing.T) {
	a, enqueued := requeueAgent(t, true)
	a.maybeRequeuePreemptedReview(context.Background(), reviewRunWith(true), requeueKw(1))
	if len(*enqueued) != 1 || (*enqueued)[0].requeueAttempts != 1 {
		t.Fatalf("enqueued %+v", *enqueued)
	}
}

func TestCompletedReviewDoesNotRequeue(t *testing.T) {
	a, enqueued := requeueAgent(t, true)
	a.maybeRequeuePreemptedReview(context.Background(), reviewRunWith(false), requeueKw(1))
	if len(*enqueued) != 0 {
		t.Fatalf("enqueued %+v", *enqueued)
	}
}

func TestRequeueAttemptCap(t *testing.T) {
	a, enqueued := requeueAgent(t, true)
	a.maybeRequeuePreemptedReview(context.Background(), reviewRunWith(true), requeueKw(4))
	if len(*enqueued) != 0 {
		t.Fatalf("enqueued %+v", *enqueued)
	}
}

func TestRequeueSkipsNonManaged(t *testing.T) {
	a, enqueued := requeueAgent(t, false)
	a.maybeRequeuePreemptedReview(context.Background(), reviewRunWith(true), requeueKw(1))
	if len(*enqueued) != 0 {
		t.Fatalf("enqueued %+v", *enqueued)
	}
}

type profileProbeAgent struct{ seen [][2]string }

func (p *profileProbeAgent) spawnBackgroundReviewNow(ctx context.Context, _ backgroundReviewKwargs) {
	secret, _, _ := provider.GetSecret(ctx, "REVIEW_PROBE_KEY")
	p.seen = append(p.seen, [2]string{home.Dir(ctx), secret})
}

type drainStop struct{}

type drainWake struct{ q *reviewIdleQueue }

func (d drainWake) Wait() {
	if d.q.pendingCount() == 0 {
		panic(drainStop{})
	}
}
func (drainWake) Set()   {}
func (drainWake) Clear() {}

func TestDispatchRunsUnderTheEnqueuingProfileContext(t *testing.T) {
	root := t.TempDir()
	homes := map[string]string{}
	for name, gate := range map[string]string{"ambient": "false", "on": "true", "off": "false"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("auxiliary:\n  background_review:\n    enabled: "+gate+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		homes[name] = dir
	}
	t.Setenv("ICEBATH_HOME", homes["ambient"])
	q, clock := makeReviewQueue(true)
	agent := &profileProbeAgent{}
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	for _, name := range []string{"on", "off"} {
		ctx := home.WithOverride(context.Background(), homes[name])
		ctx = provider.WithSecretScope(ctx, map[string]string{"REVIEW_PROBE_KEY": name}, homes[name])
		q.enqueue(ctx, agent, name, cfgKw(map[string]any{}))
	}
	*clock += reviewDeferMaxAgeS(nil) + 1
	q.wake = drainWake{q}
	func() {
		defer func() {
			if _, ok := recover().(drainStop); !ok {
				t.Fatal("dispatcher left before draining")
			}
		}()
		q.run()
	}()
	if q.pendingCount() != 0 || !reflect.DeepEqual(agent.seen, [][2]string{{homes["on"], "on"}}) {
		t.Fatalf("pending %d seen %v", q.pendingCount(), agent.seen)
	}
}
