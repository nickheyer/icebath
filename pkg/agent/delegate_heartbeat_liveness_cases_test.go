package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Parent of the liveness tests holding the child in its active list
func livenessParent(env *tools.Env, child *AIAgent) *AIAgent {
	return &AIAgent{sessionID: "parent", env: env, activeChildren: []*AIAgent{child}, activeChildrenLock: &sync.Mutex{}}
}

// Bare leaf child whose activity clock reads the given wall time
func livenessChild(env *tools.Env, sessionID string, calls int, activityTS float64) *AIAgent {
	child := interruptibleChild()
	child.env, child.delegateRole, child.delegateDepth, child.sessionID, child.model = env, "leaf", 1, sessionID, "test/model"
	child.apiCallCount, child.maxIterations, child.lastActivityTS = calls, 50, activityTS
	child.iterationBudget = NewIterationBudget(50)
	return child
}

// Child conversation served by fn for this child only
func scriptChildConversation(t *testing.T, child *AIAgent, fn func(ctx context.Context) (map[string]any, error)) {
	t.Helper()
	prev := conversationLoopRun
	exited := make(chan struct{})
	var exitOnce sync.Once
	conversationLoopRun = func(ctx context.Context, a *AIAgent, userMessage any, o RunConversationOptions, apiMode string) (map[string]any, error) {
		if a != child {
			return prev(ctx, a, userMessage, o, apiMode)
		}
		defer exitOnce.Do(func() { close(exited) })
		return fn(ctx)
	}
	t.Cleanup(func() {
		awaitWorkerExit(t, exited)
		conversationLoopRun = prev
	})
}

// Waits for an abandoned worker goroutine to return so a seam restore never races its last read
func awaitWorkerExit(t *testing.T, exited <-chan struct{}) {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(40 * time.Second):
		t.Error("abandoned worker never exited")
	}
}

// Disables worktree isolation and pins the child timeout cap the way the hermes _get_child_timeout patch does
func livenessKnobs(t *testing.T, cap *float64) {
	t.Helper()
	prevWorktree, prevTimeout := createIsolatedWorktreeFn, childTimeoutFn
	createIsolatedWorktreeFn = func(context.Context, *AIAgent, string, string) map[string]string { return nil }
	childTimeoutFn = func(context.Context) *float64 { return cap }
	t.Cleanup(func() { createIsolatedWorktreeFn, childTimeoutFn = prevWorktree, prevTimeout })
}

func capSeconds(v float64) *float64 { return &v }

// Heartbeat every 10ms, stale after two idle cycles, the given stale result grace
func fastWatchdog(t *testing.T, grace float64) {
	t.Helper()
	prevInterval, prevIdle, prevGrace := delegateHeartbeatInterval, delegateHeartbeatStaleIdle, delegateStaleResultGrace
	delegateHeartbeatInterval, delegateHeartbeatStaleIdle, delegateStaleResultGrace = 10*time.Millisecond, 2, grace
	t.Cleanup(func() {
		delegateHeartbeatInterval, delegateHeartbeatStaleIdle, delegateStaleResultGrace = prevInterval, prevIdle, prevGrace
	})
}

// Conversation that blocks on release and then returns the answer with the call count
func wedgedConversation(release <-chan struct{}, answer string, calls int) func(context.Context) (map[string]any, error) {
	return func(context.Context) (map[string]any, error) {
		select {
		case <-release:
		case <-time.After(30 * time.Second):
		}
		return map[string]any{"final_response": answer, "completed": true, "api_calls": calls, "messages": []any{}}, nil
	}
}

func releaseAfter(t *testing.T, release chan struct{}, delay time.Duration) {
	t.Helper()
	var once sync.Once
	fire := func() { once.Do(func() { close(release) }) }
	timer := time.AfterFunc(delay, fire)
	t.Cleanup(func() {
		timer.Stop()
		fire()
	})
}

func runLiveness(t *testing.T, env *tools.Env, child *AIAgent, goal string) *pyjson.Object {
	t.Helper()
	return runSingleChild(tools.WithToolEnv(context.Background(), env), 0, goal, child, livenessParent(env, child), "", nil, nil)
}

func slowButLiveChild(t *testing.T, env *tools.Env, total time.Duration, advance bool, initialCalls int) *AIAgent {
	t.Helper()
	child := livenessChild(env, "live-child", initialCalls, float64(time.Now().UnixNano())/1e9)
	scriptChildConversation(t, child, func(context.Context) (map[string]any, error) {
		deadline := time.Now().Add(total)
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			if advance {
				child.TouchActivity("working")
			}
		}
		return map[string]any{"final_response": "FINISHED AFTER OUTLIVING THE CAP", "completed": true, "api_calls": initialCalls, "messages": []any{}}, nil
	})
	return child
}

func TestProgressingChildOutlivesACapShorterThanItsRuntime(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	livenessKnobs(t, capSeconds(0.4))
	child := slowButLiveChild(t, env, 1200*time.Millisecond, true, 0)
	entry := runLiveness(t, env, child, "watch the slow provider")
	duration, _ := entry.Get("duration_seconds")
	if entryText(entry, "status") != "completed" || entryText(entry, "summary") != "FINISHED AFTER OUTLIVING THE CAP" || duration.(float64) <= 0.4 {
		t.Fatalf("entry %v", entry)
	}
	if child.hardInterruptRequested.IsSet() {
		t.Fatal("a progressing child was interrupted")
	}
	unlock := icLock(child.pendingSteerLock)
	steers := len(child.steerParts())
	unlock()
	if steers != 0 {
		t.Fatalf("budget warning steered a progressing child %d times", steers)
	}
}

func TestFrozenChildIsStillAbandonedWhenTheCapElapses(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	livenessKnobs(t, capSeconds(0.4))
	child := slowButLiveChild(t, env, 1200*time.Millisecond, false, 49)
	entry := runLiveness(t, env, child, "watch the slow provider")
	errText := entryText(entry, "error")
	timeoutSeconds, _ := entry.Get("timeout_seconds")
	lastAge, _ := entry.Get("last_event_age")
	if entryText(entry, "status") != "timeout" || !containsText(errText, "no progress in that window") || timeoutSeconds != 0.4 {
		t.Fatalf("entry %v", entry)
	}
	if age, ok := lastAge.(float64); !ok || age <= 0.3 {
		t.Fatalf("last_event_age %v", lastAge)
	}
	if !child.hardInterruptRequested.IsSet() {
		t.Fatal("frozen child was not interrupted")
	}
}

func containsText(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func wedgedAfterFinalAnswer(t *testing.T, env *tools.Env) (*AIAgent, chan struct{}) {
	t.Helper()
	child := livenessChild(env, "wedged-child", 28, 1000.0)
	release := make(chan struct{})
	scriptChildConversation(t, child, wedgedConversation(release, "VERDICT", 28))
	return child, release
}

func TestStaleHeartbeatEndsTheWaitWithoutAConfiguredTimeout(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	fastWatchdog(t, delegateStaleResultGrace)
	livenessKnobs(t, nil)
	child, release := wedgedAfterFinalAnswer(t, env)
	releaseAfter(t, release, 30*time.Second)
	entry := runLiveness(t, env, child, "review")
	if entryText(entry, "status") != "timeout" || !containsText(entryText(entry, "error"), "stopped making progress") || !child.hardInterruptRequested.IsSet() {
		t.Fatalf("entry %v interrupted %v", entry, child.hardInterruptRequested.IsSet())
	}
}

func TestStaleVerdictUnderAConfiguredCapReportsTheStaleThresholdNotTheCap(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	fastWatchdog(t, delegateStaleResultGrace)
	livenessKnobs(t, capSeconds(3600))
	child, release := wedgedAfterFinalAnswer(t, env)
	releaseAfter(t, release, 30*time.Second)
	entry := runLiveness(t, env, child, "review")
	errText := entryText(entry, "error")
	timeoutSeconds, _ := entry.Get("timeout_seconds")
	if entryText(entry, "status") != "timeout" || !containsText(errText, "stopped making progress") || containsText(errText, "3600") || timeoutSeconds != 2*0.01 {
		t.Fatalf("entry %v", entry)
	}
}

func graceChild(t *testing.T, env *tools.Env, sessionID string) (*AIAgent, chan struct{}) {
	t.Helper()
	child := livenessChild(env, sessionID, 3, 1000.0)
	release := make(chan struct{})
	scriptChildConversation(t, child, wedgedConversation(release, "DONE-"+sessionID, 3))
	return child, release
}

func TestStaleVerdictCollectsAResultThatLandsWithinTheGraceWindow(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	fastWatchdog(t, 1.0)
	livenessKnobs(t, nil)
	child, release := graceChild(t, env, "grace")
	releaseAfter(t, release, 150*time.Millisecond)
	entry := runLiveness(t, env, child, "review")
	if entryText(entry, "status") != "completed" || entryText(entry, "summary") != "DONE-grace" || !child.hardInterruptRequested.IsSet() {
		t.Fatalf("entry %v interrupted %v", entry, child.hardInterruptRequested.IsSet())
	}
}

func TestStaleVerdictWithoutAResultInTheGraceWindowIsATimeout(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	fastWatchdog(t, 0.05)
	livenessKnobs(t, nil)
	child, release := graceChild(t, env, "wedged")
	releaseAfter(t, release, 5*time.Second)
	started := time.Now()
	entry := runLiveness(t, env, child, "review")
	if time.Since(started) >= 4*time.Second {
		t.Fatalf("wait took %v", time.Since(started))
	}
	if entryText(entry, "status") != "timeout" || !containsText(entryText(entry, "error"), "stopped making progress") {
		t.Fatalf("entry %v", entry)
	}
}

func TestAsyncBatchReportsWhenTheLastChildSettlesAfterItsVerdict(t *testing.T) {
	env := delegationTestEnv(t, 0, false)
	fastWatchdog(t, 1.0)
	livenessKnobs(t, nil)
	late, lateRelease := graceChild(t, env, "late")
	done, doneRelease := graceChild(t, env, "sibling")
	close(doneRelease)
	releaseAfter(t, lateRelease, 150*time.Millisecond)
	parent := &AIAgent{sessionID: "parent", env: env, activeChildrenLock: &sync.Mutex{}}
	tasks := []map[string]any{{"goal": "late"}, {"goal": "done"}}
	batch := &delegateBatch{TaskList: tasks, Children: []delegateChild{{Index: 0, Task: tasks[0], Child: late}, {Index: 1, Task: tasks[1], Child: done}},
		Parent: parent, Creds: map[string]any{"model": "test"}, TopRole: "leaf", MaxChildren: 2, LiveWriters: []delegationLiveWriter{nil, nil}, OverallStart: time.Now()}
	combined := executeAndAggregate(tools.WithToolEnv(context.Background(), env), batch, false)
	byIndex := map[any]*pyjson.Object{}
	for _, raw := range batchResults(t, combined) {
		entry := raw.(*pyjson.Object)
		idx, _ := entry.Get("task_index")
		byIndex[idx] = entry
	}
	if entryText(byIndex[0], "status") != "completed" || entryText(byIndex[0], "summary") != "DONE-late" {
		t.Fatalf("late %v", byIndex[0])
	}
	if entryText(byIndex[1], "status") != "completed" || entryText(byIndex[1], "summary") != "DONE-sibling" {
		t.Fatalf("done %v", byIndex[1])
	}
}
