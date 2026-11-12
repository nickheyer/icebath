package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

type restartFlagCase struct {
	name     string
	agent    func() *AIAgent
	apply    func(t *testing.T, a *AIAgent, armed bool, restartCount int, response any) (PhaseVerdict, *LoopState)
	wantHeld string
}

func restartFlagCases() []restartFlagCase {
	return []restartFlagCase{
		{"restart_with_redirected_messages", func() *AIAgent { return redirectAgent(50) }, applyRedirectedRestart, "last correction"},
		{"restart_with_rebuilt_messages", func() *AIAgent { return redirectAgent(50) }, applyRebuiltRestart, ""},
	}
}

func TestTurnIterationPrep(t *testing.T) {
	caseHome(t)
	t.Run("test_single_restart_still_reissues_the_iteration", func(t *testing.T) {
		for _, tc := range restartFlagCases() {
			a := tc.agent()
			v, s := tc.apply(t, a, true, 0, nil)
			if v.Action != "continue" || 50-a.iterationBudget.Used() != 1 || s.APICallCount != 0 {
				t.Fatalf("%s: verdict %q refunds %d count %d", tc.name, v.Action, 50-a.iterationBudget.Used(), s.APICallCount)
			}
			if tc.name == "restart_with_rebuilt_messages" && s.PreflightCompressionBlocked {
				t.Fatal("the rebuilt restart is the single consumer of the preflight block")
			}
		}
	})
	t.Run("test_back_to_back_restart_refunds_are_bounded", func(t *testing.T) {
		for _, tc := range restartFlagCases() {
			a := tc.agent()
			restartCount := 0
			var actions []string
			var last *LoopState
			for len(actions) < phaseTestMaxRetries+5 && (len(actions) == 0 || actions[len(actions)-1] != "break") {
				v, s := tc.apply(t, a, true, restartCount, nil)
				actions, restartCount, last = append(actions, v.Action), s.RestartCount, s
			}
			if strings.Join(actions, ",") != "continue,continue,continue,break" || 50-a.iterationBudget.Used() != phaseTestMaxRetries {
				t.Fatalf("%s: actions %v refunds %d", tc.name, actions, 50-a.iterationBudget.Used())
			}
			if reason, _ := last.TurnExitReason.(string); !strings.HasSuffix(reason, "restart_limit_exceeded") || a.pendingSteer != tc.wantHeld {
				t.Fatalf("%s: exit %v steered %q", tc.name, last.TurnExitReason, a.pendingSteer)
			}
		}
	})
	t.Run("test_response_between_restarts_resets_the_bound", func(t *testing.T) {
		for _, tc := range restartFlagCases() {
			a := tc.agent()
			restartCount := 0
			var actions []string
			for range 3 {
				for range phaseTestMaxRetries {
					v, s := tc.apply(t, a, true, restartCount, nil)
					actions, restartCount = append(actions, v.Action), s.RestartCount
				}
				v, s := tc.apply(t, a, false, restartCount, struct{}{})
				if v.Action != "fallthrough" || s.RestartCount != 0 {
					t.Fatalf("%s: verdict %q restart count %d", tc.name, v.Action, s.RestartCount)
				}
				restartCount = s.RestartCount
			}
			if strings.Join(actions, ",") != strings.TrimSuffix(strings.Repeat("continue,", 3*phaseTestMaxRetries), ",") || a.pendingSteer != "" {
				t.Fatalf("%s: actions %v steered %q", tc.name, actions, a.pendingSteer)
			}
		}
	})
}

// Agent of the iteration budget cases, a real session database and four iterations, the ratio read from config
func budgetCheckpointAgent(t *testing.T, ctx context.Context, env *tools.Env, ratio string) *AIAgent {
	t.Helper()
	if ratio != "" {
		if err := os.WriteFile(filepath.Join(env.Home, "config.yaml"), []byte("agent:\n  budget_warning_ratio: "+ratio+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := state.NewSessionDB(t.Context(), filepath.Join(t.TempDir(), "proof.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tools.InvalidateCheckCache()
	tools.ClearToolDefinitionsCache()
	a, err := NewAIAgent(tools.WithToolEnv(ctx, env), AIAgentOptions{SessionDB: db, Model: "test-model", Provider: "openai-compat", APIKey: "test",
		BaseURL: "http://127.0.0.1:1/v1", MaxIterations: 4, QuietMode: true, SkipContextFiles: true, SkipMemory: true, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func budgetCheckpointEnv(t *testing.T) *tools.Env {
	h := caseHome(t)
	return &tools.Env{Home: h, Cwd: h, Values: &sync.Map{}}
}

func TestCheckpointRequiresOptInOrDispatcherCompletionScope(t *testing.T) {
	for _, tc := range []struct {
		ratio, scope         string
		expected, kanbanNote bool
	}{
		{"null", "ordinary", false, false}, {"0", "ordinary", false, false}, {"junk", "ordinary", false, false},
		{"0.75", "ordinary", true, false}, {"null", "owner", true, true}, {"null", "non-owner", false, false},
		{"null", "child", false, false}, {"0.75", "child", true, false}, {"null", "no-completion-tool", false, false},
	} {
		t.Run(tc.ratio+" "+tc.scope, func(t *testing.T) {
			env := budgetCheckpointEnv(t)
			ctx := context.Background()
			t.Setenv("ICEBATH_KANBAN_TASK", "")
			os.Unsetenv("ICEBATH_KANBAN_TASK")
			if tc.scope != "ordinary" {
				t.Setenv("ICEBATH_KANBAN_TASK", "t_checkpoint")
			}
			switch tc.scope {
			case "non-owner":
				t.Cleanup(tools.EnterNonDispatcherOwnedContext(env))
			case "child":
				ctx = tools.WithDelegatedChildContext(ctx, true)
			}
			a := budgetCheckpointAgent(t, ctx, env, tc.ratio)
			if tc.scope == "no-completion-tool" {
				delete(a.validToolNames, "kanban_complete")
			}
			for range 3 {
				a.iterationBudget.Consume()
			}
			rows := []map[string]any{{"role": "user", "content": "work"},
				{"role": "assistant", "tool_calls": []any{map[string]any{"id": "t", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}},
				{"role": "tool", "tool_call_id": "t", "content": "verified artifact"}}
			original := messagesToRows(rowsToMessages(rows))
			s := &LoopState{Messages: rowsToMessages(rows), APICallCount: 3}
			if err := prepareIteration(tools.WithToolEnv(ctx, env), a, s); err != nil {
				t.Fatal(err)
			}
			got := messagesToRows(s.Messages)
			last, _ := got[2]["content"].(string)
			if !reflect.DeepEqual(got[:2], original[:2]) || (last != "verified artifact") != tc.expected || strings.Contains(last, "kanban_complete") != tc.kanbanNote {
				t.Fatalf("rows %v", got)
			}
			if err := prepareIteration(tools.WithToolEnv(ctx, env), a, s); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(messagesToRows(s.Messages), got) || a.iterationBudget.Remaining() != 1 {
				t.Fatalf("second pass rows %v remaining %d", messagesToRows(s.Messages), a.iterationBudget.Remaining())
			}
		})
	}
}

func preparedIteration(t *testing.T, rows []map[string]any, userMessage any, idx int) (*AIAgent, *LoopState) {
	t.Helper()
	env := budgetCheckpointEnv(t)
	a := budgetCheckpointAgent(t, context.Background(), env, "")
	ctx := tools.WithToolEnv(context.Background(), env)
	resetPerTurnAgentState(ctx, a)
	s := &LoopState{Messages: rowsToMessages(rows), APICallCount: 1, UserMessage: userMessage, CurrentTurnUserIdx: idx}
	if err := prepareIteration(ctx, a, s); err != nil {
		t.Fatal(err)
	}
	return a, s
}

func TestPrepareIterationReanchorsAfterTheRepairMergesRows(t *testing.T) {
	rows := []map[string]any{
		{"role": "assistant", "content": "**Context snapshot**"},
		{"role": "user", "content": "compaction summary written as a user row"},
		{"role": "user", "content": "first protected user message"},
		{"role": "assistant", "content": "ok", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "t", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "c1", "content": "out"},
		{"role": "user", "content": "NEW question"},
	}
	recorded := len(rows) - 1
	a, s := preparedIteration(t, rows, "NEW question", recorded)
	if len(s.Messages) >= len(rows)+1 || recorded < len(s.Messages) {
		t.Fatalf("the repair kept %d of %d rows", len(s.Messages), len(rows))
	}
	if s.Messages[s.CurrentTurnUserIdx].Content != "NEW question" || a.persistUserMessageIdx == nil || *a.persistUserMessageIdx != s.CurrentTurnUserIdx {
		t.Fatalf("index %d row %v persist anchor %v", s.CurrentTurnUserIdx, s.Messages[s.CurrentTurnUserIdx].Content, a.persistUserMessageIdx)
	}
}

func TestToolTailRowIsKeptAndNeutralised(t *testing.T) {
	const legacy, placeholder = "[response interrupted]", "[interrupt: no assistant output for this turn]"
	hidden := func() map[string]any {
		return map[string]any{"role": "assistant", "content": "", "display_kind": "hidden", "api_content": legacy}
	}
	checkpoint := "[Context from the interrupted assistant response]\nhalf a poem\n\nmake it short"
	redirect := []map[string]any{{"role": "user", "content": "write a poem"}, hidden(), {"role": "user", "content": "make it short", "api_content": checkpoint}}
	_, s := preparedIteration(t, redirect, "make it short", 2)
	if redirect[1]["api_content"] != legacy || len(s.Messages) != 3 {
		t.Fatalf("stored rows rewritten or dropped: %v -> %d rows", redirect, len(s.Messages))
	}
	if roles := fmt.Sprint(s.Messages[0].Role, s.Messages[1].Role, s.Messages[2].Role); roles != "userassistantuser" ||
		s.Messages[1].APIContent != placeholder || s.Messages[2].APIContent != checkpoint {
		t.Fatalf("roles %s hidden %q checkpoint %q", roles, s.Messages[1].APIContent, s.Messages[2].APIContent)
	}

	rows := []map[string]any{
		{"role": "user", "content": "edit the file"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "patch", "arguments": "{}"}}}},
		{"role": "tool", "tool_call_id": "c1", "content": "ok edited"},
		hidden(),
		{"role": "user", "content": "they do! increase the timing"},
	}
	live := rowsToMessages(rows)
	env := budgetCheckpointEnv(t)
	a := budgetCheckpointAgent(t, context.Background(), env, "")
	ctx := tools.WithToolEnv(context.Background(), env)
	resetPerTurnAgentState(ctx, a)
	st := &LoopState{Messages: append([]*state.Message{}, live...), APICallCount: 1, UserMessage: "they do! increase the timing", CurrentTurnUserIdx: 4}
	if err := prepareIteration(ctx, a, st); err != nil {
		t.Fatal(err)
	}
	if live[3].APIContent != legacy {
		t.Fatal("the durable row was rewritten in place")
	}
	var hiddenRows []*state.Message
	for i, m := range st.Messages {
		if m.Role == "assistant" && m.DisplayKind == "hidden" {
			hiddenRows = append(hiddenRows, m)
		}
		if m.Role == "tool" && i+1 < len(st.Messages) && st.Messages[i+1].Role == "user" {
			t.Fatalf("tool then user adjacency at %d", i)
		}
	}
	if len(hiddenRows) != 1 || hiddenRows[0].Content != "" || hiddenRows[0].APIContent != placeholder {
		t.Fatalf("hidden rows %+v", hiddenRows)
	}
}

func toolCallBudgets(a *AIAgent, requestedCap int) []int {
	st := &truncState{a: a}
	kwargs := map[string]any{}
	if requestedCap != 0 {
		kwargs["max_tokens"] = requestedCap
	}
	var budgets []int
	for range 4 {
		v, err := st.retryTruncatedToolCall(context.Background(), kwargs)
		if err != nil || v.Action != "continue" {
			panic(fmt.Sprintf("truncated tool call retry %q %v", v.Action, err))
		}
		budgets = append(budgets, a.ephemeralMaxOutputTokens)
	}
	return budgets
}

func lengthContinuationBudgets(a *AIAgent, requestedCap int) []int {
	kwargs := map[string]any{}
	if requestedCap != 0 {
		kwargs["max_tokens"] = requestedCap
	}
	var budgets []int
	for n := 1; n <= 4; n++ {
		s := &LoopState{Retry: &TurnRetryState{RestartWithLengthContinuation: true}, UserMessage: "hi", APIKwargs: kwargs, MaxRetries: 3,
			APICallCount: 1, LengthContinueRetries: n, TurnExitReason: "unknown"}
		v, err := applyRetryRestarts(context.Background(), a, s)
		if err != nil || v.Action != "continue" {
			panic(fmt.Sprintf("length continuation restart %q %v", v.Action, err))
		}
		budgets = append(budgets, a.ephemeralMaxOutputTokens)
	}
	return budgets
}

func TestTruncatedToolCallBoost(t *testing.T) {
	caseHome(t)
	sites := map[string]func(*AIAgent, int) []int{"tool call": toolCallBudgets, "length continuation": lengthContinuationBudgets}
	t.Run("test_retry_raises_budget_above_large_requested_cap", func(t *testing.T) {
		for name, site := range sites {
			for _, requested := range []int{32768, 65536} {
				budgets := site(&AIAgent{}, requested)
				highest := 0
				for _, b := range budgets {
					highest = max(highest, b)
				}
				if budgets[0] <= requested || highest > requested*2 {
					t.Fatalf("%s requested %d budgets %v", name, requested, budgets)
				}
			}
		}
	})
	t.Run("test_boost_clamped_to_known_model_output_limit", func(t *testing.T) {
		want := []int{64000, 64000, 64000, 64000}
		for name, site := range sites {
			for _, requested := range []int{40000, 64000} {
				if budgets := site(&AIAgent{apiMode: "anthropic_messages", model: "claude-sonnet-4-5"}, requested); !reflect.DeepEqual(budgets, want) {
					t.Fatalf("%s requested %d budgets %v", name, requested, budgets)
				}
			}
		}
	})
	t.Run("test_small_explicit_max_tokens_ladder_still_capped_at_floor", func(t *testing.T) {
		if budgets := toolCallBudgets(&AIAgent{maxTokens: iptr(4096)}, 0); !reflect.DeepEqual(budgets, []int{8192, 16384, 32768, 32768}) {
			t.Fatalf("budgets %v", budgets)
		}
	})
}
