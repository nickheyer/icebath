package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const phaseTestMaxRetries = 3

// One iteration's restart handling with the rebuilt flag armed or a model response when rebuilt is false
func applyRebuiltRestart(t *testing.T, a *AIAgent, rebuilt bool, restartCount int, response any) (PhaseVerdict, *LoopState) {
	t.Helper()
	s := &LoopState{Retry: &TurnRetryState{RestartWithRebuiltMessages: rebuilt}, Response: response, UserMessage: "hi",
		APIKwargs: map[string]any{}, MaxRetries: phaseTestMaxRetries, APICallCount: 1, RestartCount: restartCount,
		PreflightCompressionBlocked: true, TurnExitReason: "unknown"}
	v, err := applyRetryRestarts(context.Background(), a, s)
	if err != nil {
		t.Fatal(err)
	}
	return v, s
}

func budgetAgent(used int) *AIAgent {
	b := NewIterationBudget(100)
	for range used {
		b.Consume()
	}
	return &AIAgent{iterationBudget: b}
}

func TestSingleRebuiltRestartStillReissuesTheIteration(t *testing.T) {
	a := budgetAgent(10)
	v, s := applyRebuiltRestart(t, a, true, 0, nil)
	if v.Action != "continue" || a.iterationBudget.Used() != 9 || s.APICallCount != 0 || s.PreflightCompressionBlocked {
		t.Fatalf("verdict %q used %d count %d blocked %v", v.Action, a.iterationBudget.Used(), s.APICallCount, s.PreflightCompressionBlocked)
	}
}

func TestBackToBackRebuiltRestartRefundsAreBounded(t *testing.T) {
	a := budgetAgent(20)
	restartCount := 0
	var actions []string
	var last *LoopState
	for len(actions) < phaseTestMaxRetries+5 && (len(actions) == 0 || actions[len(actions)-1] != "break") {
		v, s := applyRebuiltRestart(t, a, true, restartCount, nil)
		actions, restartCount, last = append(actions, v.Action), s.RestartCount, s
	}
	want := []string{"continue", "continue", "continue", "break"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("actions %v", actions)
	}
	if refunds := 20 - a.iterationBudget.Used(); refunds != phaseTestMaxRetries {
		t.Fatalf("refunds %d", refunds)
	}
	if reason, _ := last.TurnExitReason.(string); !strings.HasSuffix(reason, "restart_limit_exceeded") {
		t.Fatalf("exit reason %v", last.TurnExitReason)
	}
}

func TestResponseBetweenRebuiltRestartsResetsTheBound(t *testing.T) {
	a := budgetAgent(50)
	restartCount := 0
	var actions []string
	for range 3 {
		for range phaseTestMaxRetries {
			v, s := applyRebuiltRestart(t, a, true, restartCount, nil)
			actions, restartCount = append(actions, v.Action), s.RestartCount
		}
		v, s := applyRebuiltRestart(t, a, false, restartCount, struct{}{})
		if v.Action != "fallthrough" || s.RestartCount != 0 {
			t.Fatalf("verdict %q restart count %d", v.Action, s.RestartCount)
		}
		restartCount = s.RestartCount
	}
	for _, act := range actions {
		if act != "continue" {
			t.Fatalf("actions %v", actions)
		}
	}
}

func TestScriptedPreludeCallIDIsUniquePerChat(t *testing.T) {
	args := map[string]any{"beat": "name"}
	first, _ := preludeCallID("chat-1", "setup_beat", args, 1)
	same, _ := preludeCallID("chat-1", "setup_beat", map[string]any{"beat": "name"}, 1)
	other, _ := preludeCallID("chat-2", "setup_beat", args, 1)
	if first != same {
		t.Fatal("same chat, card and place must give the same id")
	}
	if first == other {
		t.Fatal("another chat must give another id")
	}
}

func TestNeutraliseReplayEchoGhostsCopiesHazardRows(t *testing.T) {
	ghost := &state.Message{Role: "assistant", DisplayKind: "hidden", Content: "  " + LegacyInterruptedPlaceholder + " "}
	current := &state.Message{Role: "assistant", DisplayKind: "hidden", Content: InterruptedPlaceholder}
	seq := []*state.Message{{Role: "user", Content: "hi"}, ghost, current}
	out, n := neutraliseReplayEchoGhosts(seq)
	if n != 1 || out[1] == ghost || out[1].Content != "" || out[1].APIContent != InterruptedPlaceholder || out[2] != current {
		t.Fatalf("neutralised %d out %+v", n, out[1])
	}
	if ghost.Content == "" {
		t.Fatal("the original row must stay untouched")
	}
	if same, n := neutraliseReplayEchoGhosts(seq[2:]); n != 0 || &same[0] != &seq[2] {
		t.Fatal("a sequence without hazards is returned as is")
	}
}

func TestIterationBudgetWarningAppendsToNewestToolResult(t *testing.T) {
	a := budgetAgent(8)
	a.iterationBudget.MaxTotal = 10
	a.budgetWarningRatio = 0.8
	msgs := []*state.Message{{Role: "tool", Content: "result"}}
	if !maybeInjectIterationBudgetWarning(a, msgs) || !a.iterationBudgetWarningInjected {
		t.Fatal("warning not injected at the ratio")
	}
	if text, _ := msgs[0].Content.(string); !strings.HasPrefix(text, "result\n\n[SYSTEM NOTICE") || !strings.Contains(text, "used 8 of 10") {
		t.Fatalf("content %q", text)
	}
	if maybeInjectIterationBudgetWarning(a, msgs) {
		t.Fatal("warning is one shot")
	}
	b := budgetAgent(8)
	b.iterationBudget.MaxTotal, b.budgetWarningRatio = 10, 0.8
	persisted := []*state.Message{state.MessageFromDict(map[string]any{"role": "tool", "content": "r", dbPersistedMarker: true})}
	if maybeInjectIterationBudgetWarning(b, persisted) {
		t.Fatal("a persisted tool row is never mutated")
	}
}

func TestPreviousToolRoundPairsResults(t *testing.T) {
	msgs := []*state.Message{
		{Role: "assistant", ToolCalls: []state.ToolCall{{ID: "c1", Function: state.ToolCallFunction{Name: "read", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	}
	round := previousToolRound(msgs)
	if len(round) != 1 || round[0]["name"] != "read" || round[0]["result"] != "ok" || round[0]["arguments"] != "{}" {
		t.Fatalf("round %v", round)
	}
}
