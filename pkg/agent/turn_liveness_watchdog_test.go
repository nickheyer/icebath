package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

type watchdogDB struct {
	mu           sync.Mutex
	events       [][]string
	refreshTimes []float64
}

func (d *watchdogDB) add(e ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, e)
}

func (d *watchdogDB) snapshot() ([][]string, []float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]string(nil), d.events...), append([]float64(nil), d.refreshTimes...)
}

func (d *watchdogDB) GetSession(_ context.Context, sessionID string) (*pyjson.Object, error) {
	return pyjson.ObjectOf("id", sessionID), nil
}

func (d *watchdogDB) AcquireSessionTurnLease(sessionID, holder string, _ state.TurnLeaseWait) (bool, error) {
	d.add("acquire", sessionID, holder)
	return true, nil
}

func (d *watchdogDB) ResolveResumeSessionID(sessionID string) (string, error) {
	d.add("resolve", sessionID)
	return sessionID, nil
}

func (d *watchdogDB) GetMessagesAsConversation(_ context.Context, sessionID string, _ state.ConversationOptions) ([]map[string]any, error) {
	d.add("reload", sessionID)
	return []map[string]any{{"role": "user", "content": "durable latest"}}, nil
}

func (d *watchdogDB) RefreshSessionTurnLease(sessionID, holder string, _ float64, _ *float64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, []string{"refresh", sessionID, holder})
	d.refreshTimes = append(d.refreshTimes, leaseWallNow())
	return true, nil
}

func (d *watchdogDB) ReleaseSessionTurnLease(sessionID, holder string) error {
	d.add("release", sessionID, holder)
	return nil
}

// Agent entering a new turn with a stale activity clock from an idle previous turn
func watchdogAgent(t *testing.T, db *watchdogDB) *AIAgent {
	t.Helper()
	stores := useLeaseStores(t)
	interval := 60.0
	a := &AIAgent{sessionID: "stalled-session", platform: "desktop", model: "test-model", quietMode: true,
		suppressStatusOutput: true, hardInterruptRequested: &pythread.Event{}, pendingRedirectLock: &sync.Mutex{}}
	a.sessionDBCreated = true
	a.sessionTurnLeaseRefreshInterval = &interval
	a.lastActivityTS = activityWallNow() - 1000.0
	a.lastActivityDesc = "previous turn (idle)"
	stores[a] = db
	return a
}

// Arms the watchdog fast through the agent.turn_liveness config section
func watchdogConfig(t *testing.T) {
	t.Helper()
	prev := turnLivenessConfig
	turnLivenessConfig = func(context.Context) map[string]any {
		return map[string]any{"agent": map[string]any{"turn_liveness": map[string]any{"timeout_s": 0.3, "poll_s": 0.05}}}
	}
	t.Cleanup(func() { turnLivenessConfig = prev })
}

// Drives the facade with a fake inner conversation loop
func runWatchdogTurn(t *testing.T, a *AIAgent, loop func(a *AIAgent, history []map[string]any) map[string]any) map[string]any {
	t.Helper()
	prev := conversationLoopRun
	conversationLoopRun = func(_ context.Context, a *AIAgent, _ any, o RunConversationOptions, _ string) (map[string]any, error) {
		return loop(a, o.ConversationHistory), nil
	}
	t.Cleanup(func() { conversationLoopRun = prev })
	result, err := a.RunConversation(context.Background(), "new message", RunConversationOptions{
		ConversationHistory: []map[string]any{{"role": "user", "content": "stale"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func watchdogFired(records []loggerRecord, sessionID string) bool {
	for _, r := range records {
		if strings.Contains(r.msg, "Turn liveness watchdog fired") && strings.Contains(r.msg, sessionID) {
			return true
		}
	}
	return false
}

func lastEventKind(events [][]string) string {
	if len(events) == 0 {
		return ""
	}
	return events[len(events)-1][0]
}

func TestWatchdogForceAbortsSilentlyStalledTurn(t *testing.T) {
	watchdogConfig(t)
	db := &watchdogDB{}
	a := watchdogAgent(t, db)
	records := captureLogger(t, "agent.turn_liveness", slog.LevelError)
	seen := ""
	result := runWatchdogTurn(t, a, func(a *AIAgent, history []map[string]any) map[string]any {
		if !a.hardInterruptRequested.Wait(30.0) {
			t.Error("watchdog did not publish a hard interrupt")
		}
		seen = a.interruptMessage
		return map[string]any{"final_response": "aborted", "messages": history, "api_calls": 0, "completed": false, "interrupted": true}
	})
	if result["interrupted"] != true || result["final_response"] != "aborted" {
		t.Fatalf("result %v", result)
	}
	if !watchdogFired(records(), "stalled-session") {
		t.Fatal("the stall was not surfaced")
	}
	if a.interruptMessage != "" || !strings.Contains(seen, "no progress") {
		t.Fatalf("interrupt message left %q seen %q", a.interruptMessage, seen)
	}
	events, _ := db.snapshot()
	if last := events[len(events)-1]; last[0] != "release" || last[1] != "stalled-session" {
		t.Fatalf("events %v", events)
	}
}

func TestWatchdogDoesNotFireWhileTurnStillMakingProgress(t *testing.T) {
	watchdogConfig(t)
	db := &watchdogDB{}
	a := watchdogAgent(t, db)
	interval := 0.05
	a.sessionTurnLeaseRefreshInterval = &interval
	records := captureLogger(t, "agent.turn_liveness", slog.LevelError)
	start := time.Now()
	result := runWatchdogTurn(t, a, func(a *AIAgent, history []map[string]any) map[string]any {
		for time.Since(start) < 600*time.Millisecond {
			a.TouchActivity("test tick")
			time.Sleep(20 * time.Millisecond)
		}
		return map[string]any{"final_response": "done", "messages": history, "api_calls": 1, "completed": true}
	})
	if result["completed"] != true || result["interrupted"] == true || a.interruptRequested.Load() {
		t.Fatalf("result %v interrupted %v", result, a.interruptRequested.Load())
	}
	if watchdogFired(records(), "") {
		t.Fatal("the watchdog fired on a progressing turn")
	}
	if _, refreshes := db.snapshot(); len(refreshes) < 1 {
		t.Fatal("the lease never renewed")
	}
}

func TestWatchdogStopsLeaseRenewalWhenInterruptCannotUnwindWedge(t *testing.T) {
	watchdogConfig(t)
	db := &watchdogDB{}
	a := watchdogAgent(t, db)
	interval := 0.05
	a.sessionTurnLeaseRefreshInterval = &interval
	start := time.Now()
	var fireAt float64
	result := runWatchdogTurn(t, a, func(a *AIAgent, history []map[string]any) map[string]any {
		for !a.hardInterruptRequested.IsSet() && time.Since(start) < 10*time.Second {
			time.Sleep(5 * time.Millisecond)
		}
		fireAt = leaseWallNow()
		for leaseWallNow()-fireAt < 0.5 {
			time.Sleep(5 * time.Millisecond)
		}
		return map[string]any{"final_response": "recovered", "messages": history, "api_calls": 0, "completed": true}
	})
	if result["completed"] != true || time.Since(start) >= 10*time.Second || fireAt == 0 {
		t.Fatalf("result %v after %v", result, time.Since(start))
	}
	events, refreshes := db.snapshot()
	for _, at := range refreshes {
		if at > fireAt+0.15 {
			t.Fatalf("lease renewal continued after watchdog fired at %v: %v", fireAt, refreshes)
		}
	}
	if lastEventKind(events) != "release" {
		t.Fatalf("events %v", events)
	}
}

func assertDeclinedTurnKeptItsLease(t *testing.T, db *watchdogDB, records []loggerRecord) {
	t.Helper()
	if !watchdogFired(records, "stalled-session") {
		t.Fatal("the stall was not surfaced")
	}
	events, refreshes := db.snapshot()
	if len(refreshes) < 1 || lastEventKind(events) != "release" {
		t.Fatalf("refreshes %v events %v", refreshes, events)
	}
}

func TestWatchdogDeclinesAbortWhenActivityResumesDuringWarning(t *testing.T) {
	watchdogConfig(t)
	db := &watchdogDB{}
	a := watchdogAgent(t, db)
	interval := 0.05
	a.sessionTurnLeaseRefreshInterval = &interval
	resume, touched := &pythread.Event{}, &pythread.Event{}
	a.statusCallback = func(kind string, _ any) {
		if kind == "warn" {
			resume.Set()
			if !touched.Wait(10.0) {
				t.Error("the turn never resumed")
			}
		}
	}
	records := captureLogger(t, "agent.turn_liveness", slog.LevelError)
	start := time.Now()
	var requested bool
	runWatchdogTurn(t, a, func(a *AIAgent, history []map[string]any) map[string]any {
		if !resume.Wait(10.0) {
			t.Error("the warning never started")
		}
		a.TouchActivity("turn resumed")
		touched.Set()
		for time.Since(start) < 800*time.Millisecond {
			a.TouchActivity("still alive")
			time.Sleep(20 * time.Millisecond)
		}
		requested = a.interruptRequested.Load()
		return map[string]any{"final_response": "recovered", "messages": history, "api_calls": 1, "completed": true}
	})
	if time.Since(start) >= 10*time.Second || requested {
		t.Fatalf("turn was hard-interrupted while resumed %v", requested)
	}
	recs := records()
	for _, r := range recs {
		if strings.Contains(r.msg, "watchdog aborted turn") || strings.Contains(r.msg, "Force-aborting") {
			t.Fatalf("declined abort published a committed-abort settlement %q", r.msg)
		}
	}
	assertDeclinedTurnKeptItsLease(t, db, recs)
}

func TestWatchdogDeclinesAbortWhenActivityResumesAfterRevalidation(t *testing.T) {
	watchdogConfig(t)
	db := &watchdogDB{}
	a := watchdogAgent(t, db)
	interval := 0.05
	a.sessionTurnLeaseRefreshInterval = &interval
	var injected sync.Map
	count := 0
	prev := turnLeaseInterrupt
	turnLeaseInterrupt = func(a *AIAgent, message string, o InterruptOptions) (bool, error) {
		count++
		injected.Store(count, true)
		a.TouchActivity("resumed after revalidation")
		return a.InterruptWith(message, o), nil
	}
	t.Cleanup(func() { turnLeaseInterrupt = prev })
	records := captureLogger(t, "agent.turn_liveness", slog.LevelError)
	start := time.Now()
	requested := false
	result := runWatchdogTurn(t, a, func(a *AIAgent, history []map[string]any) map[string]any {
		for time.Since(start) < time.Second {
			if a.hardInterruptRequested.IsSet() {
				requested = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		return map[string]any{"final_response": "recovered", "messages": history, "api_calls": 1, "completed": true}
	})
	if _, ok := injected.Load(1); !ok || result["completed"] != true || result["interrupted"] == true || requested || a.interruptRequested.Load() {
		t.Fatalf("result %v requested %v", result, requested)
	}
	assertDeclinedTurnKeptItsLease(t, db, records())
}

type blockingCommitFence struct {
	entered, release *pythread.Event
	mu               sync.Mutex
	calls            int
}

func (f *blockingCommitFence) CommitInFlight() bool { return true }

func (f *blockingCommitFence) CancelBeforeCommit(any) bool {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	f.entered.Set()
	if !f.release.Wait(10.0) {
		panic("fence was never released")
	}
	return true
}

func TestWatchdogDeclinesAbortWhenActivityResumesInsideInterruptPublication(t *testing.T) {
	watchdogConfig(t)
	db := &watchdogDB{}
	a := watchdogAgent(t, db)
	interval := 0.05
	a.sessionTurnLeaseRefreshInterval = &interval
	fence := &blockingCommitFence{entered: &pythread.Event{}, release: &pythread.Event{}}
	a.activeCompressionCommitFence = fence
	records := captureLogger(t, "agent.turn_liveness", slog.LevelError)
	start := time.Now()
	var requested, hardEvent, toolSignal bool
	var message string
	result := runWatchdogTurn(t, a, func(a *AIAgent, history []map[string]any) map[string]any {
		if !fence.entered.Wait(10.0) {
			t.Error("interrupt never reached the fence")
		}
		a.TouchActivity("resumed inside interrupt publication window")
		fence.release.Set()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			a.TouchActivity("still alive")
			if a.hardInterruptRequested.IsSet() {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		requested, hardEvent = a.interruptRequested.Load(), a.hardInterruptRequested.IsSet()
		toolSignal, message = a.interruptThreadSignalPending, a.interruptMessage
		return map[string]any{"final_response": "recovered", "messages": history, "api_calls": 1, "completed": true}
	})
	fence.mu.Lock()
	calls := fence.calls
	fence.mu.Unlock()
	if time.Since(start) >= 10*time.Second || calls != 1 || result["completed"] != true || result["interrupted"] == true {
		t.Fatalf("fence admissions %d result %v", calls, result)
	}
	if requested || hardEvent || toolSignal || message != "" {
		t.Fatalf("published against a stale generation requested %v hard %v signal %v message %q", requested, hardEvent, toolSignal, message)
	}
	assertDeclinedTurnKeptItsLease(t, db, records())
}

func TestWatchdogDeclinesAbortWhenInterruptPublishRaises(t *testing.T) {
	watchdogConfig(t)
	db := &watchdogDB{}
	a := watchdogAgent(t, db)
	interval := 0.05
	a.sessionTurnLeaseRefreshInterval = &interval
	var raised sync.Map
	prev := turnLeaseInterrupt
	turnLeaseInterrupt = func(*AIAgent, string, InterruptOptions) (bool, error) {
		raised.Store("raised", true)
		return false, errors.New("synthetic interrupt publication failure")
	}
	t.Cleanup(func() { turnLeaseInterrupt = prev })
	records := captureLogger(t, "agent.turn_liveness", slog.LevelError)
	start := time.Now()
	result := runWatchdogTurn(t, a, func(a *AIAgent, history []map[string]any) map[string]any {
		for time.Since(start) < time.Second {
			time.Sleep(5 * time.Millisecond)
		}
		return map[string]any{"final_response": "recovered", "messages": history, "api_calls": 1, "completed": true}
	})
	if _, ok := raised.Load("raised"); !ok || result["completed"] != true || result["interrupted"] == true {
		t.Fatalf("result %v", result)
	}
	if a.interruptRequested.Load() || a.interruptMessage != "" {
		t.Fatalf("exception fallback mutated interrupt state %v %q", a.interruptRequested.Load(), a.interruptMessage)
	}
	assertDeclinedTurnKeptItsLease(t, db, records())
}

type parkingReleaseLock struct {
	inner         sync.Locker
	parkOnRelease int64
	releaseCount  atomic.Int64
	parked        *pythread.Event
	releasePark   *pythread.Event
}

func (l *parkingReleaseLock) Lock() { l.inner.Lock() }

func (l *parkingReleaseLock) Unlock() {
	l.inner.Unlock()
	if n := l.releaseCount.Add(1); l.parkOnRelease != 0 && n == l.parkOnRelease {
		l.parked.Set()
		if !l.releasePark.Wait(10.0) {
			panic("parked activity-lock release was never released")
		}
	}
}

func TestDeclinedAbortDoesNotCancelPendingCompressionCommit(t *testing.T) {
	a := &AIAgent{quietMode: true, hardInterruptRequested: &pythread.Event{}}
	a.turnLivenessActivityGeneration = 5
	fence := NewCompressionCommitFence()
	a.activeCompressionCommitFence = fence
	lock := &parkingReleaseLock{inner: &sync.Mutex{}, parkOnRelease: 1, parked: &pythread.Event{}, releasePark: &pythread.Event{}}
	a.turnLivenessActivityLocker = lock
	var ret bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		generation := int64(5)
		ret = a.InterruptWith("watchdog: no progress", InterruptOptions{HardCancel: true, RequireGeneration: &generation})
	}()
	if !lock.parked.Wait(10.0) {
		t.Fatal("interrupt never reached the claim-reservation boundary")
	}
	touched := make(chan struct{})
	go func() {
		defer close(touched)
		a.TouchActivity("turn resumed")
	}()
	select {
	case <-touched:
	case <-time.After(10 * time.Second):
		t.Fatal("competing activity never landed")
	}
	if a.turnLivenessActivityGeneration != 6 {
		t.Fatalf("generation %d", a.turnLivenessActivityGeneration)
	}
	lock.releasePark.Set()
	<-done
	if ret || a.interruptRequested.Load() || a.hardInterruptRequested.IsSet() {
		t.Fatalf("interrupt should have declined ret %v requested %v", ret, a.interruptRequested.Load())
	}
	if !fence.BeginCommit(nil) {
		t.Fatal("declined liveness abort left the pending compression fence cancelled")
	}
	fence.FinishCommit()
}
