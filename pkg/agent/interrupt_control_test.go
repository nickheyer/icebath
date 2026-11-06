package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Agent carrying only the interrupt and steer slots, matching a hermes agent built without __init__
func bareSteerAgent() *AIAgent {
	return &AIAgent{pendingSteerLock: &sync.Mutex{}, pendingRedirectLock: &sync.Mutex{},
		modelRequestActive: &pythread.Event{}, activeChildrenLock: &sync.Mutex{}, quietMode: true, apiMode: "chat_completions"}
}

func TestSteerDrainReturnsAndClears(t *testing.T) {
	a := bareSteerAgent()
	a.Steer("hello")
	if got := a.drainPendingSteer(); got != "hello" || a.pendingSteer != "" {
		t.Fatalf("drain %q pending %q", got, a.pendingSteer)
	}
}

func TestWithdrawTakesBackOneUnconsumedTicketAndKeepsTheRest(t *testing.T) {
	a := bareSteerAgent()
	a.Steer("first")
	a.SteerTicket("  two\nlines  ", "t2")
	a.Steer("two")
	a.SteerTicket("last", "t4")
	if !a.WithdrawSteer("t2") || a.pendingSteer != "first\ntwo\nlast" {
		t.Fatalf("pending %q", a.pendingSteer)
	}
	if a.WithdrawSteer("missing") {
		t.Fatal("unknown ticket withdrawn")
	}
	if !a.WithdrawSteer("t4") || a.pendingSteer != "first\ntwo" {
		t.Fatalf("pending %q", a.pendingSteer)
	}
}

func TestWithdrawAfterADrainTookTheTicketIsRefused(t *testing.T) {
	a := bareSteerAgent()
	a.SteerTicket("alpha", "a")
	if a.drainPendingSteer() != "alpha" {
		t.Fatal("drain")
	}
	a.SteerTicket("alpha", "b")
	if a.WithdrawSteer("a") {
		t.Fatal("a drain consumed a")
	}
	if !a.WithdrawSteer("b") || a.pendingSteer != "" {
		t.Fatalf("pending %q", a.pendingSteer)
	}
}

func TestADrainedSteerPutBackUndeliveredKeepsItsTicket(t *testing.T) {
	a := bareSteerAgent()
	a.Steer("note")
	a.SteerTicket("queued prompt", "t")
	requeuePendingSteer(a, a.drainPendingSteer())
	if a.pendingSteer != "note\nqueued prompt" {
		t.Fatalf("pending %q", a.pendingSteer)
	}
	if !a.WithdrawSteer("t") || a.pendingSteer != "note" {
		t.Fatalf("pending %q", a.pendingSteer)
	}
}

func TestTheLeftoverDrainHandsBackOnlyTicketlessSteers(t *testing.T) {
	a := bareSteerAgent()
	a.Steer("typed /steer")
	a.SteerTicket("queued prompt", "t")
	if got := a.drainLeftoverSteer(); got != "typed /steer" || a.pendingSteer != "queued prompt" {
		t.Fatalf("leftover %q pending %q", got, a.pendingSteer)
	}
	if got := a.drainLeftoverSteer(); got != "" {
		t.Fatalf("second leftover %q", got)
	}
	if !a.WithdrawSteer("t") || a.pendingSteer != "" {
		t.Fatalf("pending %q", a.pendingSteer)
	}
}

func TestAHardStopDropsATicketUnread(t *testing.T) {
	a := bareSteerAgent()
	a.SteerTicket("queued prompt", "t")
	a.clearInterruptWith(false, true)
	if a.pendingSteer != "" || !a.WithdrawSteer("t") {
		t.Fatalf("pending %q", a.pendingSteer)
	}
}

func TestRedirectRejectsWhenNoTurnIsActive(t *testing.T) {
	a := bareSteerAgent()
	if a.Redirect("change course") || a.pendingRedirect != "" {
		t.Fatal("redirect accepted without a live request")
	}
}

func TestRedirectCancelsOnlyAnActiveModelRequest(t *testing.T) {
	a := bareSteerAgent()
	a.modelRequestActive.Set()
	if !a.Redirect("use Postgres") || a.pendingRedirect != "use Postgres" || !a.interruptRequested || a.interruptMessage != "" {
		t.Fatalf("redirect %q interrupted %v", a.pendingRedirect, a.interruptRequested)
	}
}

func TestMultipleRedirectsPreserveMessageBoundaries(t *testing.T) {
	a := bareSteerAgent()
	a.modelRequestActive.Set()
	a.Redirect("first correction")
	a.Redirect("second correction")
	first, second := strings.Index(a.pendingRedirect, "first correction"), strings.Index(a.pendingRedirect, "second correction")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("pending %q", a.pendingRedirect)
	}
}

func TestHardInterruptWinsOverNewRedirect(t *testing.T) {
	a := bareSteerAgent()
	a.modelRequestActive.Set()
	a.interruptRequested = true
	if a.Redirect("too late") || a.pendingRedirect != "" {
		t.Fatal("redirect accepted over a pending stop")
	}
}

func TestResponseCompletionBeforeRedirectLockRejectsCorrection(t *testing.T) {
	a := bareSteerAgent()
	a.modelRequestActive.Set()
	started := make(chan struct{})
	accepted := make(chan bool, 1)
	a.pendingRedirectLock.Lock()
	go func() {
		close(started)
		accepted <- a.Redirect("late correction")
	}()
	<-started
	a.modelRequestActive.Clear()
	a.pendingRedirectLock.Unlock()
	if <-accepted {
		t.Fatal("late correction accepted")
	}
	if a.hasPendingRedirect() {
		t.Fatal("redirect left pending")
	}
}

func TestHardStopWinsConcurrentRedirect(t *testing.T) {
	a := bareSteerAgent()
	a.modelRequestActive.Set()
	var start, done sync.WaitGroup
	start.Add(1)
	done.Add(2)
	go func() { defer done.Done(); start.Wait(); a.Redirect("change course") }()
	go func() { defer done.Done(); start.Wait(); a.Interrupt("stop requested") }()
	start.Done()
	done.Wait()
	if !a.interruptRequested || a.interruptMessage != "stop requested" || a.pendingRedirect != "" {
		t.Fatalf("interrupted %v message %q redirect %q", a.interruptRequested, a.interruptMessage, a.pendingRedirect)
	}
}

type fakeCodexControl struct{ calls []string }

func (f *fakeCodexControl) RequestInterrupt() { f.calls = append(f.calls, "interrupt") }
func (f *fakeCodexControl) RequestSteer(text string) (bool, error) {
	f.calls = append(f.calls, "steer:"+text)
	return true, nil
}

func TestCodexAppServerHardStopReachesNativeSession(t *testing.T) {
	fake := &fakeCodexControl{}
	prev := codexTurnControlFor
	codexTurnControlFor = func(*AIAgent) codexTurnControl { return fake }
	t.Cleanup(func() { codexTurnControlFor = prev })
	a := bareSteerAgent()
	a.apiMode = "codex_app_server"
	a.Interrupt("")
	if !slices.Equal(fake.calls, []string{"interrupt"}) {
		t.Fatalf("calls %v", fake.calls)
	}
}

func TestRedirectDuringToolExecutionUsesSafeSteerBoundary(t *testing.T) {
	a := bareSteerAgent()
	a.executingTools = true
	if !a.Redirect("also check migrations") || a.pendingRedirect != "" || a.pendingSteer != "also check migrations" || a.interruptRequested {
		t.Fatalf("redirect %q steer %q", a.pendingRedirect, a.pendingSteer)
	}
}

func TestConcurrentSteerCallsPreserveAllText(t *testing.T) {
	a := bareSteerAgent()
	const n = 200
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); a.Steer(fmt.Sprintf("note-%d", i)) }()
	}
	wg.Wait()
	lines := strings.Split(a.drainPendingSteer(), "\n")
	seen := map[string]bool{}
	for _, l := range lines {
		seen[l] = true
	}
	if len(lines) != n || len(seen) != n || !seen["note-0"] || !seen["note-199"] {
		t.Fatalf("lines %d unique %d", len(lines), len(seen))
	}
}

func TestHardCancelDropsPendingSteer(t *testing.T) {
	a := bareSteerAgent()
	a.interruptRequested = true
	a.Steer("will be dropped")
	a.pendingRedirect = "also drop this"
	a.clearInterruptWith(false, true)
	if a.pendingSteer != "" || a.pendingRedirect != "" {
		t.Fatalf("steer %q redirect %q", a.pendingSteer, a.pendingRedirect)
	}
}

func TestSoftClearPreservesPendingSteer(t *testing.T) {
	a := bareSteerAgent()
	a.interruptRequested = true
	a.Steer("must survive the rebuild")
	a.pendingRedirect = "correction"
	a.clearInterrupt(true)
	if a.pendingSteer != "must survive the rebuild" || a.pendingRedirect != "correction" {
		t.Fatalf("steer %q redirect %q", a.pendingSteer, a.pendingRedirect)
	}
	a.clearInterrupt(false)
	if a.pendingSteer != "must survive the rebuild" || a.pendingRedirect != "" {
		t.Fatalf("steer %q redirect %q", a.pendingSteer, a.pendingRedirect)
	}
}

func TestPreAPIDrainAppendsUserRowAndLeavesToolRowUntouched(t *testing.T) {
	a := bareSteerAgent()
	toolRow := &state.Message{Role: "tool", Content: "output here", ToolCallID: "tc1"}
	before := *toolRow
	messages := []*state.Message{
		{Role: "user", Content: "do something"},
		state.MessageFromDict(map[string]any{"role": "assistant", "content": "ok", "tool_calls": []any{
			map[string]any{"id": "tc1", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}}),
		toolRow,
	}
	a.Steer("focus on error handling")
	injectSteerAfterNewestToolResult(a, &messages, a.drainPendingSteer())
	last, _ := messages[len(messages)-1].Content.(string)
	if messages[len(messages)-2] != toolRow || toolRow.Content != before.Content || toolRow.ToolCallID != before.ToolCallID {
		t.Fatal("tool row changed")
	}
	if messages[len(messages)-1].Role != "user" || !strings.Contains(last, SteerMarkerOpen) || !strings.Contains(last, "focus on error handling") || a.pendingSteer != "" {
		t.Fatalf("steer row %+v", messages[len(messages)-1])
	}
}

func TestInterruptExitReasonNamesTheSystemIssuer(t *testing.T) {
	cases := map[string]string{
		"explicit stop requested":  "interrupted_by_user",
		"user sent a new message":  "interrupted_by_user",
		"":                         "interrupted_by_user",
		"cron inactivity watchdog": "interrupted_by_system(cron_inactivity_watchdog)",
		"turn liveness watchdog":   "interrupted_by_system(turn_liveness_watchdog)",
	}
	for reason, want := range cases {
		a := &AIAgent{interruptRequested: true, toolInterruptReason: reason, quietMode: true}
		s := &LoopState{OriginalUserMessage: "hi", TurnExitReason: "unknown"}
		v, err := beginIteration(context.Background(), a, s)
		if err != nil || v.Action != "break" || !s.Interrupted || s.TurnExitReason != want {
			t.Fatalf("%q: %q %v %v %v", reason, v.Action, s.Interrupted, s.TurnExitReason, err)
		}
	}
}

// One iteration's restart handling with the redirected flag armed and a correction always pending
func applyRedirectedRestart(t *testing.T, a *AIAgent, redirected bool, restartCount int, response any) (PhaseVerdict, *LoopState) {
	t.Helper()
	a.pendingRedirect = "last correction"
	s := &LoopState{Retry: &TurnRetryState{RestartWithRedirectedMessages: redirected}, Response: response, UserMessage: "hi",
		APIKwargs: map[string]any{}, MaxRetries: phaseTestMaxRetries, APICallCount: 1, RestartCount: restartCount,
		PreflightCompressionBlocked: true, TurnExitReason: "unknown"}
	v, err := applyRetryRestarts(context.Background(), a, s)
	if err != nil {
		t.Fatal(err)
	}
	return v, s
}

func redirectAgent(used int) *AIAgent {
	a := budgetAgent(used)
	a.pendingSteerLock, a.pendingRedirectLock = &sync.Mutex{}, &sync.Mutex{}
	return a
}

func TestSingleRedirectedRestartStillReissuesTheIteration(t *testing.T) {
	a := redirectAgent(10)
	v, s := applyRedirectedRestart(t, a, true, 0, nil)
	if v.Action != "continue" || a.iterationBudget.Used() != 9 || s.APICallCount != 0 {
		t.Fatalf("verdict %q used %d count %d", v.Action, a.iterationBudget.Used(), s.APICallCount)
	}
}

func TestBackToBackRedirectedRestartRefundsAreBounded(t *testing.T) {
	a := redirectAgent(20)
	restartCount := 0
	var actions []string
	for len(actions) == 0 || actions[len(actions)-1] != "break" {
		v, s := applyRedirectedRestart(t, a, true, restartCount, nil)
		actions, restartCount = append(actions, v.Action), s.RestartCount
	}
	if strings.Join(actions, ",") != "continue,continue,continue,break" || 20-a.iterationBudget.Used() != phaseTestMaxRetries {
		t.Fatalf("actions %v used %d", actions, a.iterationBudget.Used())
	}
	if a.pendingSteer != "last correction" {
		t.Fatalf("steered %q", a.pendingSteer)
	}
}

func TestResponseBetweenRedirectedRestartsResetsTheBound(t *testing.T) {
	a := redirectAgent(50)
	restartCount := 0
	for range 3 {
		for range phaseTestMaxRetries {
			v, s := applyRedirectedRestart(t, a, true, restartCount, nil)
			if v.Action != "continue" {
				t.Fatalf("action %q", v.Action)
			}
			restartCount = s.RestartCount
		}
		v, s := applyRedirectedRestart(t, a, false, restartCount, struct{}{})
		if v.Action != "fallthrough" || s.RestartCount != 0 {
			t.Fatalf("verdict %q restart count %d", v.Action, s.RestartCount)
		}
		restartCount = s.RestartCount
	}
	if a.pendingSteer != "" {
		t.Fatalf("steered %q", a.pendingSteer)
	}
}
