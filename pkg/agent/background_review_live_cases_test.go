package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Live agent carrying what the review spawn and the turn wrapper read, the hermes _bare_agent
func brLiveAgent(t *testing.T) *AIAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &AIAgent{
		model: "fake-model", platform: "telegram", provider: "openai", sessionID: "test-session",
		memoryEnabled: true, quietMode: true, requestOverrides: map[string]any{},
		systemPromptState: systemPromptState{cachedSystemPrompt: "test-cached-system-prompt"},
		sessionStart:      time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		backgroundReviewState: backgroundReviewState{reviewPrompts: map[string]string{
			"_MEMORY_REVIEW_PROMPT": "review memory", "_SKILL_REVIEW_PROMPT": "review skills", "_COMBINED_REVIEW_PROMPT": "review both",
		}},
		backgroundReviewLock: &sync.Mutex{}, activeChildrenLock: &sync.Mutex{},
		hardInterruptRequested: &pythread.Event{},
		env:                    &tools.Env{Values: &sync.Map{}},
	}
}

// Fork fields a recorded review fork needs to run its turn and take an interrupt
func brForkInit(fork *AIAgent) {
	fork.env = &tools.Env{Values: &sync.Map{}}
	fork.quietMode = true
	fork.requestOverrides = map[string]any{}
	fork.pendingRedirectLock = &sync.Mutex{}
	fork.hardInterruptRequested = &pythread.Event{}
	fork.endSessionOnClose = true
}

// Replaces the fork conversation for the test
func brForkConversation(t *testing.T, fn func(ctx context.Context, fork *AIAgent, userMessage string, history []map[string]any) error) {
	t.Helper()
	prev := runReviewForkConversation
	runReviewForkConversation = fn
	t.Cleanup(func() { runReviewForkConversation = prev })
}

// Captures review workers instead of starting them, the hermes CapturingThread
func brCaptureWorkers(t *testing.T) *[]func() {
	t.Helper()
	targets := &[]func(){}
	prev := startReviewWorker
	startReviewWorker = func(fn func()) { *targets = append(*targets, fn) }
	t.Cleanup(func() { startReviewWorker = prev })
	return targets
}

// Runs review workers inline at spawn, the hermes ImmediateThread
func brImmediateWorkers(t *testing.T) {
	t.Helper()
	prev := startReviewWorker
	startReviewWorker = func(fn func()) { fn() }
	t.Cleanup(func() { startReviewWorker = prev })
}

// Records fork client releases, blocking while hold is set for the fork block selects
func brRecordReleases(t *testing.T, onRelease func(fork *AIAgent)) {
	t.Helper()
	prev := releaseForkClientsFn
	releaseForkClientsFn = func(_ context.Context, fork *AIAgent) { onRelease(fork) }
	t.Cleanup(func() { releaseForkClientsFn = prev })
}

func brSnapshot() []map[string]any { return []map[string]any{{"role": "user", "content": "hello"}} }

// Polls cond up to timeout seconds like Event.wait on a flag
func brWaitFor(timeout float64, cond func() bool) bool {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

var errTurnBoundaryReached = errors.New("turn boundary reached")

// Stops a live turn exactly when it reaches turn context construction
func brInstallLiveTurnBoundary(t *testing.T, onBoundary func()) {
	t.Helper()
	prev := buildTurnContextFn
	buildTurnContextFn = func(context.Context, *AIAgent, TurnContextRequest) (*TurnContext, error) {
		if onBoundary != nil {
			onBoundary()
		}
		return nil, errTurnBoundaryReached
	}
	t.Cleanup(func() { buildTurnContextFn = prev })
}

// Outcome of one wrapped live turn
type brLiveResult struct {
	boundary bool
	err      error
	ret      map[string]any
}

func brRunLiveTurn(a *AIAgent) brLiveResult {
	ret, err := a.RunConversation(context.Background(), "next turn", RunConversationOptions{TaskID: "live-task"})
	if errors.Is(err, errTurnBoundaryReached) {
		return brLiveResult{boundary: true}
	}
	return brLiveResult{ret: ret, err: err}
}

// One relay call with whether the review had acknowledged by then
type brRelayCall struct {
	name  string
	acked bool
}

// Relay coordinator recording the acquire and begin calls around the real coordinator
type brRelayRecorder struct {
	real  relayTurnCoordinator
	run   *backgroundReviewRun
	mu    sync.Mutex
	calls []brRelayCall
}

func (c *brRelayRecorder) record(name string) {
	c.mu.Lock()
	c.calls = append(c.calls, brRelayCall{name, c.run != nil && c.run.requestDone.IsSet()})
	c.mu.Unlock()
}

func (c *brRelayRecorder) snapshot() []brRelayCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]brRelayCall(nil), c.calls...)
}

func (c *brRelayRecorder) AcquireConversation(ctx context.Context, o tools.RelayConversation) *tools.ConversationLease {
	c.record("acquire")
	return c.real.AcquireConversation(ctx, o)
}

func (c *brRelayRecorder) BeginTurn(ctx context.Context, lease *tools.ConversationLease, turnID, taskID string, metadata map[string]any) (*tools.RelayTurnContext, error) {
	c.record("begin")
	return c.real.BeginTurn(ctx, lease, turnID, taskID, metadata)
}

func (c *brRelayRecorder) FinishLogicalCalls(ctx context.Context, turn *tools.RelayTurnContext, outcome string) {
	c.real.FinishLogicalCalls(ctx, turn, outcome)
}

func (c *brRelayRecorder) EndTurn(ctx context.Context, turn *tools.RelayTurnContext, outcome string) {
	c.real.EndTurn(ctx, turn, outcome)
}

func (c *brRelayRecorder) ReleaseConversation(lease *tools.ConversationLease) {
	c.real.ReleaseConversation(lease)
}

// Installs the relay recorder and the task run recorder, the hermes _install_relay_recorder
func brInstallRelayRecorder(t *testing.T, run *backgroundReviewRun) *brRelayRecorder {
	t.Helper()
	rec := &brRelayRecorder{real: tools.RelayCoordinator, run: run}
	prevCoord, prevStart := relayCoordinatorFn, startTaskRunFn
	relayCoordinatorFn = func() relayTurnCoordinator { return rec }
	startTaskRunFn = func(context.Context, *tools.Env, string, string, string, string) { rec.record("start_task_run") }
	t.Cleanup(func() { relayCoordinatorFn, startTaskRunFn = prevCoord, prevStart })
	return rec
}

func brSameCalls(got []brRelayCall, want ...brRelayCall) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestBackgroundReviewReleasesClientsWithoutClosingSharedSession(t *testing.T) {
	var events []string
	rec := recordForks(t, func(fork *AIAgent) { brForkInit(fork); events = append(events, "init") })
	brForkConversation(t, func(context.Context, *AIAgent, string, []map[string]any) error {
		events = append(events, "run_conversation")
		return nil
	})
	brRecordReleases(t, func(*AIAgent) { events = append(events, "release_clients") })
	brImmediateWorkers(t)
	agent := brLiveAgent(t)
	agent.spawnBackgroundReview(context.Background(), brSnapshot(), true, false, nil, false)
	if strings.Join(events, ",") != "init,run_conversation,release_clients" {
		t.Fatalf("events %v", events)
	}
	if len(rec.forks) != 1 || rec.forks[0].memoryProviderShutdown {
		t.Fatalf("fork closed its shared session: %d forks", len(rec.forks))
	}
}

func TestBackgroundReviewForkOptsOutOfSessionFinalization(t *testing.T) {
	seen := map[string]bool{}
	rec := recordForks(t, brForkInit)
	brForkConversation(t, func(_ context.Context, fork *AIAgent, _ string, _ []map[string]any) error {
		seen["at_run_time"] = fork.endSessionOnClose
		return nil
	})
	brImmediateWorkers(t)
	agent := brLiveAgent(t)
	agent.spawnBackgroundReview(context.Background(), brSnapshot(), true, false, nil, false)
	if len(rec.forks) != 1 || rec.forks[0].endSessionOnClose {
		t.Fatal("fork still owns the parent's session row")
	}
	if ran, ok := seen["at_run_time"]; !ok || ran {
		t.Fatalf("at run time %v %v", seen, ok)
	}
}

func TestBackgroundReviewSkippedInDelegationSubagent(t *testing.T) {
	rec := recordForks(t, brForkInit)
	brForkConversation(t, func(context.Context, *AIAgent, string, []map[string]any) error { return nil })
	brImmediateWorkers(t)
	agent := brLiveAgent(t)
	agent.delegateDepth = 1
	agent.spawnBackgroundReview(context.Background(), brSnapshot(), true, true, nil, false)
	if len(rec.opts) != 0 {
		t.Fatal("no review fork should be spawned inside a subagent")
	}
}

func TestBackgroundReviewDisabledSkipsAutomaticSpawn(t *testing.T) {
	rec := recordForks(t, brForkInit)
	brForkConversation(t, func(context.Context, *AIAgent, string, []map[string]any) error { return nil })
	brImmediateWorkers(t)
	prev := loadBackgroundReviewSettings
	loadBackgroundReviewSettings = func(context.Context) (bool, map[string]any) { return false, map[string]any{"enabled": false} }
	t.Cleanup(func() { loadBackgroundReviewSettings = prev })
	agent := brLiveAgent(t)
	agent.delegateDepth = 0
	agent.spawnBackgroundReview(context.Background(), brSnapshot(), true, false, nil, false)
	if len(rec.opts) != 0 {
		t.Fatal("automatic review must not spawn when disabled")
	}
	focus := "save the deploy workflow"
	agent.spawnBackgroundReview(context.Background(), brSnapshot(), true, false, &focus, false)
	if len(rec.opts) != 1 {
		t.Fatalf("/refine must still run when enabled=false, forks %d", len(rec.opts))
	}
}

func TestBackgroundReviewExplicitFocusRunsEvenInSubagent(t *testing.T) {
	rec := recordForks(t, brForkInit)
	brForkConversation(t, func(context.Context, *AIAgent, string, []map[string]any) error { return nil })
	brImmediateWorkers(t)
	agent := brLiveAgent(t)
	agent.delegateDepth = 2
	focus := "save the deploy workflow as a skill"
	agent.spawnBackgroundReview(context.Background(), brSnapshot(), false, true, &focus, false)
	if len(rec.opts) != 1 {
		t.Fatalf("explicit focus review must run even in a subagent, forks %d", len(rec.opts))
	}
}

func TestBackgroundReviewRegistersBeforeStartRunsAndCleansUp(t *testing.T) {
	recordForks(t, brForkInit)
	agent := brLiveAgent(t)
	var seenRun *backgroundReviewRun
	var seenChildren []*AIAgent
	var seenFork *AIAgent
	brForkConversation(t, func(_ context.Context, fork *AIAgent, _ string, _ []map[string]any) error {
		seenRun = currentBackgroundReviewRun(agent)
		seenChildren = agent.activeChildrenSnapshot()
		seenFork = agent.backgroundReviewAgent
		return nil
	})
	targets := brCaptureWorkers(t)
	agent.spawnBackgroundReview(context.Background(), brSnapshot(), true, false, nil, false)
	run := currentBackgroundReviewRun(agent)
	if run == nil || len(*targets) != 1 || run.requestDone.IsSet() {
		t.Fatalf("run %v targets %d", run, len(*targets))
	}
	(*targets)[0]()
	if seenFork == nil || seenRun != run || len(seenChildren) != 1 || seenChildren[0] != seenFork {
		t.Fatalf("fork %v run %v children %v", seenFork, seenRun, seenChildren)
	}
	if !run.requestDone.IsSet() || !run.requestFinished {
		t.Fatal("request exit not published once")
	}
	if currentBackgroundReviewRun(agent) != nil || agent.backgroundReviewAgent != nil || len(agent.activeChildrenSnapshot()) != 0 {
		t.Fatal("review state not cleaned up")
	}
}

func TestLiveTurnWaitsForReviewExitBeforeRelayAndTurnContext(t *testing.T) {
	var reviewEntered, reviewReturned, allowReviewReturn, waitStarted pythread.Event
	seen := map[string]any{}
	var seenMu sync.Mutex
	rec := recordForks(t, brForkInit)
	brForkConversation(t, func(context.Context, *AIAgent, string, []map[string]any) error {
		reviewEntered.Set()
		if !allowReviewReturn.Wait(2.0) {
			t.Error("review never allowed to return")
		}
		reviewReturned.Set()
		return nil
	})
	targets := brCaptureWorkers(t)
	agent := brLiveAgent(t)
	agent.spawnBackgroundReview(context.Background(), brSnapshot(), true, false, nil, false)
	run := currentBackgroundReviewRun(agent)
	if run == nil {
		t.Fatal("no review run")
	}
	prevAck := awaitReviewAck
	awaitReviewAck = func(r *backgroundReviewRun, timeout float64) bool {
		waitStarted.Set()
		return r.requestDone.Wait(timeout)
	}
	t.Cleanup(func() { awaitReviewAck = prevAck })
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); (*targets)[0]() }()
	if !reviewEntered.Wait(2.0) {
		t.Fatal("review never entered")
	}
	var boundaryReached pythread.Event
	brInstallLiveTurnBoundary(t, func() {
		seenMu.Lock()
		seen["review_returned_at_boundary"] = reviewReturned.IsSet()
		seenMu.Unlock()
		boundaryReached.Set()
	})
	relay := brInstallRelayRecorder(t, run)
	liveDone := make(chan brLiveResult, 1)
	go func() { liveDone <- brRunLiveTurn(agent) }()
	fork := rec.forks[0]
	if !brWaitFor(2.0, func() bool { return fork.interruptRequested.Load() }) {
		t.Fatal("fork not interrupted")
	}
	started := waitStarted.Wait(2.0)
	before := relay.snapshot()
	allowReviewReturn.Set()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish")
	}
	var live brLiveResult
	select {
	case live = <-liveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("live turn did not finish")
	}
	if !started || len(before) != 0 {
		t.Fatalf("wait started %v relay calls before ack %v", started, before)
	}
	if calls := relay.snapshot(); !brSameCalls(calls, brRelayCall{"acquire", true}, brRelayCall{"begin", true}, brRelayCall{"start_task_run", true}) {
		t.Fatalf("relay calls %v", calls)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if !boundaryReached.IsSet() || fork.interruptMessage != "superseded by a new live turn" || seen["review_returned_at_boundary"] != true || !live.boundary {
		t.Fatalf("boundary %v message %q seen %v live %+v", boundaryReached.IsSet(), fork.interruptMessage, seen, live)
	}
}

func TestLiveTurnCancelsReviewDuringStartupBeforeProvider(t *testing.T) {
	var providerCalls []string
	recordForks(t, brForkInit)
	brForkConversation(t, func(_ context.Context, _ *AIAgent, userMessage string, _ []map[string]any) error {
		providerCalls = append(providerCalls, userMessage)
		return nil
	})
	targets := brCaptureWorkers(t)
	agent := brLiveAgent(t)
	agent.spawnBackgroundReview(context.Background(), brSnapshot(), true, false, nil, false)
	run := currentBackgroundReviewRun(agent)
	if run == nil {
		t.Fatal("no review run")
	}
	var boundaryReached pythread.Event
	brInstallLiveTurnBoundary(t, boundaryReached.Set)
	relay := brInstallRelayRecorder(t, run)
	liveDone := make(chan brLiveResult, 1)
	go func() { liveDone <- brRunLiveTurn(agent) }()
	if !run.cancelRequested.Wait(2.0) {
		t.Fatal("cancel never requested")
	}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); (*targets)[0]() }()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish")
	}
	var live brLiveResult
	select {
	case live = <-liveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("live turn did not finish")
	}
	if !boundaryReached.IsSet() || len(providerCalls) != 0 || !run.requestDone.IsSet() || !live.boundary {
		t.Fatalf("boundary %v provider %v done %v live %+v", boundaryReached.IsSet(), providerCalls, run.requestDone.IsSet(), live)
	}
	if calls := relay.snapshot(); !brSameCalls(calls, brRelayCall{"acquire", true}, brRelayCall{"begin", true}, brRelayCall{"start_task_run", true}) {
		t.Fatalf("relay calls %v", calls)
	}
}

func TestLiveTurnProceedsWhenReviewAcknowledgementTimesOut(t *testing.T) {
	var reviewEntered, allowReviewReturn pythread.Event
	rec := recordForks(t, brForkInit)
	brForkConversation(t, func(context.Context, *AIAgent, string, []map[string]any) error {
		reviewEntered.Set()
		allowReviewReturn.Wait(5.0)
		return nil
	})
	targets := brCaptureWorkers(t)
	prevTimeout := backgroundReviewCancelTimeoutSeconds
	backgroundReviewCancelTimeoutSeconds = 0.01
	t.Cleanup(func() { backgroundReviewCancelTimeoutSeconds = prevTimeout })
	agent := brLiveAgent(t)
	agent.spawnBackgroundReview(context.Background(), brSnapshot(), true, false, nil, false)
	run := currentBackgroundReviewRun(agent)
	if run == nil {
		t.Fatal("no review run")
	}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); (*targets)[0]() }()
	if !reviewEntered.Wait(2.0) {
		t.Fatal("review never entered")
	}
	fork := rec.forks[0]
	fork.pendingRedirectLock.Lock()
	var boundaryCalls []bool
	brInstallLiveTurnBoundary(t, func() { boundaryCalls = append(boundaryCalls, true) })
	relay := brInstallRelayRecorder(t, run)
	started := time.Now()
	live := brRunLiveTurn(agent)
	elapsed := time.Since(started)
	interruptedBeforeRelease := fork.interruptRequested.Load()
	fork.pendingRedirectLock.Unlock()
	allowReviewReturn.Set()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish")
	}
	if elapsed >= 2*time.Second || interruptedBeforeRelease {
		t.Fatalf("live turn took %v, fork interrupted before the wedge released %v", elapsed, interruptedBeforeRelease)
	}
	if !brWaitFor(2.0, func() bool { return fork.interruptRequested.Load() }) {
		t.Fatal("wedged interrupt never returned")
	}
	if len(boundaryCalls) != 1 || !live.boundary {
		t.Fatalf("boundary %v live %+v", boundaryCalls, live)
	}
	if calls := relay.snapshot(); !brSameCalls(calls, brRelayCall{"acquire", false}, brRelayCall{"begin", false}, brRelayCall{"start_task_run", false}) {
		t.Fatalf("relay calls %v", calls)
	}
	if agent.sessionID != "test-session" {
		t.Fatalf("session %q", agent.sessionID)
	}
}

func TestLiveTurnInterruptsLegacyReviewButKeepsForegroundPriority(t *testing.T) {
	agent := brLiveAgent(t)
	legacy := &AIAgent{quietMode: true, requestOverrides: map[string]any{}, env: &tools.Env{Values: &sync.Map{}},
		hardInterruptRequested: &pythread.Event{}}
	agent.backgroundReviewRun = nil
	agent.backgroundReviewAgent = legacy
	var boundaryCalls []bool
	brInstallLiveTurnBoundary(t, func() { boundaryCalls = append(boundaryCalls, true) })
	relay := brInstallRelayRecorder(t, nil)
	live := brRunLiveTurn(agent)
	if !brWaitFor(2.0, func() bool { return legacy.interruptRequested.Load() }) {
		t.Fatal("legacy review never interrupted")
	}
	if legacy.interruptMessage != "superseded by a new live turn" {
		t.Fatalf("interrupt message %q", legacy.interruptMessage)
	}
	if len(boundaryCalls) != 1 || !live.boundary {
		t.Fatalf("boundary %v live %+v", boundaryCalls, live)
	}
	if calls := relay.snapshot(); !brSameCalls(calls, brRelayCall{"acquire", false}, brRelayCall{"begin", false}, brRelayCall{"start_task_run", false}) {
		t.Fatalf("relay calls %v", calls)
	}
	if agent.sessionID != "test-session" {
		t.Fatalf("session %q", agent.sessionID)
	}
}

func TestStaleReviewCleanupCannotClearOrSignalNewerReview(t *testing.T) {
	var firstCleanupEntered, allowFirstCleanup pythread.Event
	rec := recordForks(t, brForkInit)
	brForkConversation(t, func(context.Context, *AIAgent, string, []map[string]any) error { return nil })
	brRecordReleases(t, func(fork *AIAgent) {
		if len(rec.forks) > 0 && fork == rec.forks[0] {
			firstCleanupEntered.Set()
			if !allowFirstCleanup.Wait(2.0) {
				t.Error("first cleanup never released")
			}
		}
	})
	targets := brCaptureWorkers(t)
	agent := brLiveAgent(t)
	agent.spawnBackgroundReview(context.Background(), []map[string]any{{"role": "user", "content": "first"}}, true, false, nil, false)
	firstRun := currentBackgroundReviewRun(agent)
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); (*targets)[0]() }()
	if !firstCleanupEntered.Wait(2.0) || !firstRun.requestDone.IsSet() {
		t.Fatal("first review did not publish its request exit before cleanup")
	}
	agent.spawnBackgroundReview(context.Background(), []map[string]any{{"role": "user", "content": "second"}}, true, false, nil, false)
	secondRun := currentBackgroundReviewRun(agent)
	if len(*targets) != 2 || secondRun == nil || secondRun == firstRun || secondRun.requestDone.IsSet() {
		t.Fatalf("second run %v targets %d", secondRun, len(*targets))
	}
	allowFirstCleanup.Set()
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first worker did not finish")
	}
	if currentBackgroundReviewRun(agent) != secondRun || secondRun.requestDone.IsSet() {
		t.Fatal("stale cleanup cleared or signalled the newer review")
	}
	(*targets)[1]()
	if !secondRun.requestDone.IsSet() || currentBackgroundReviewRun(agent) != nil {
		t.Fatal("second review did not finish cleanly")
	}
}

func TestSkillPatchOffSilentVerboseShowsDiff(t *testing.T) {
	ctx := context.Background()
	review := func() []map[string]any {
		return skillCallMessages(t,
			map[string]any{"action": "patch", "name": "demo", "old_string": "a", "new_string": "b"},
			map[string]any{"success": true, "message": "Patched SKILL.md in skill 'demo' (1 replacement).", "_change": map[string]any{"old": "a", "new": "b"}})
	}
	off, err := summarizeBackgroundReviewActions(ctx, review(), nil, "off")
	if err != nil || len(off) != 0 {
		t.Fatalf("off %v %v", off, err)
	}
	verbose, err := summarizeBackgroundReviewActions(ctx, review(), nil, "verbose")
	if err != nil || len(verbose) != 1 || !strings.Contains(verbose[0], "demo") || !strings.Contains(verbose[0], "→") {
		t.Fatalf("verbose %v %v", verbose, err)
	}
}

// Live agent carrying the whole interrupt control surface so the real interrupt path runs
func brInterruptScopedAgent(t *testing.T) *AIAgent {
	t.Helper()
	a := brLiveAgent(t)
	a.pendingRedirectLock = &sync.Mutex{}
	a.pendingSteerLock = &sync.Mutex{}
	a.toolWorkerThreads = map[int64]bool{}
	a.toolWorkerThreadsLock = &sync.Mutex{}
	return a
}

func TestSupersedeHardInterruptTargetsReviewForkOnly(t *testing.T) {
	parent := brInterruptScopedAgent(t)
	fork := brInterruptScopedAgent(t)
	foregroundID, reviewID := tools.NewWorkerID(), tools.NewWorkerID()
	t.Cleanup(func() {
		tools.SetInterrupt(reviewID, false, "")
		tools.SetInterrupt(foregroundID, false, "")
	})
	parent.executionThreadID = int64(foregroundID)
	fork.executionThreadID = int64(reviewID)
	parent.backgroundReviewAgent = nil
	run := prepareBackgroundReviewRun(parent)
	if run == nil || !run.beginRequest(fork) {
		t.Fatal("review request not admitted")
	}
	ackDone := make(chan struct{})
	go func() {
		defer close(ackDone)
		brWaitFor(1.5, func() bool { return tools.IsWorkerInterrupted(reviewID) })
		finishBackgroundReviewRun(parent, run)
	}()
	cancelBackgroundReviewForLiveTurn(parent)
	select {
	case <-ackDone:
	case <-time.After(3 * time.Second):
		t.Fatal("acknowledgement never came")
	}
	ctx := context.Background()
	if !fork.interruptRequested.Load() || fork.toolInterruptReason != "background review superseded" ||
		!fork.hardInterruptRequested.IsSet() || !tools.IsWorkerInterrupted(reviewID) ||
		tools.InterruptReason(tools.WithWorker(ctx, reviewID)) != "background review superseded" {
		t.Fatalf("fork flags requested %v reason %q hard %v worker %v", fork.interruptRequested.Load(), fork.toolInterruptReason,
			fork.hardInterruptRequested.IsSet(), tools.IsWorkerInterrupted(reviewID))
	}
	if parent.interruptRequested.Load() || parent.toolInterruptReason != "" || parent.hardInterruptRequested.IsSet() ||
		parent.interruptThreadSignalPending || tools.IsWorkerInterrupted(foregroundID) ||
		tools.InterruptReason(tools.WithWorker(ctx, foregroundID)) != "" {
		t.Fatalf("foreground touched: requested %v reason %q hard %v pending %v worker %v", parent.interruptRequested.Load(),
			parent.toolInterruptReason, parent.hardInterruptRequested.IsSet(), parent.interruptThreadSignalPending,
			tools.IsWorkerInterrupted(foregroundID))
	}
}

// Captures every log line at info level and above for the test
func brCaptureInfoLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestTurnExitLogCarriesForkOriginTag(t *testing.T) {
	logs := brCaptureInfoLog(t)
	live, fork := brLiveAgent(t), brLiveAgent(t)
	fork.turnOrigin = "background_review"
	for _, a := range []*AIAgent{live, fork} {
		a.maxIterations = 10
		a.iterationBudget.Store(nil)
	}
	messages := []*state.Message{{Role: "user"}, {Role: "assistant"}}
	logTurnExit(live, messages, "a", 1, "text_response", false)
	logTurnExit(fork, messages, "a", 1, "interrupted_during_api_call(background_review_superseded)", true)
	var records []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, "Turn ended") {
			records = append(records, line)
		}
	}
	if len(records) != 2 {
		t.Fatalf("records %q", records)
	}
	if strings.Contains(records[0], "origin=") {
		t.Fatalf("live record carries an origin: %s", records[0])
	}
	if !strings.Contains(records[1], "origin=background_review") {
		t.Fatalf("fork record lacks its origin: %s", records[1])
	}
}
