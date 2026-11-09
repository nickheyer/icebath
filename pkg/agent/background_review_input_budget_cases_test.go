package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One scripted provider answer, a read_file tool call carrying prompt tokens or the final text
type brScriptedResponse struct {
	promptTokens int
	final        bool
}

// Loopback provider replaying scripted chat completions in order and counting the requests it served
func brScriptedProvider(t *testing.T, filePath string, responses []brScriptedResponse) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "test/model", "object": "model"}}})
			return
		}
		_, _ = io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		n := int(calls.Add(1)) - 1
		if n >= len(responses) {
			t.Errorf("provider call %d past the %d scripted responses", n+1, len(responses))
			http.Error(w, "no more scripted responses", http.StatusInternalServerError)
			return
		}
		resp := responses[n]
		var message map[string]any
		var usage any
		finish := "tool_calls"
		if resp.final {
			message = map[string]any{"role": "assistant", "content": "done", "tool_calls": nil}
			finish = "stop"
		} else {
			args, _ := json.Marshal(map[string]any{"path": filePath})
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_1", "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": string(args)}}}}
			usage = map[string]any{"prompt_tokens": resp.promptTokens, "completion_tokens": 1, "total_tokens": resp.promptTokens + 1}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-budget", "object": "chat.completion", "model": "test/model", "usage": usage,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// Real agent over the scripted provider whose tool loop reads one temp file per tool call
func brLoopAgent(t *testing.T, responses []brScriptedResponse) (*AIAgent, context.Context, *atomic.Int32) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	filePath := filepath.Join(h, "notes.txt")
	if err := os.WriteFile(filePath, []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, calls := brScriptedProvider(t, filePath, responses)
	env := &tools.Env{Home: h, Cwd: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{Model: "test/model", APIKey: "test-key-1234567890", BaseURL: srv.URL + "/v1", Platform: "cli",
		MaxIterations: 10, QuietMode: true, SkipMemory: true, SkipContextFiles: true, Cwd: h, Env: env, EnabledToolsets: []string{"file"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.DisableStreaming()
	a.compressionEnabled = false
	return a, ctx, calls
}

func TestReviewInputBudgetStopsToolLoopBeforeNextProviderCall(t *testing.T) {
	a, ctx, calls := brLoopAgent(t, []brScriptedResponse{{promptTokens: 50_000}, {promptTokens: 50_000}, {promptTokens: 50_000}, {final: true}})
	a.reviewInputTokenBudget = 100_000
	result, err := a.RunConversation(ctx, "do some tool work", RunConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected the loop to stop after crossing the input budget, but %d provider calls were made (budget %d, used %d)",
			calls.Load(), a.reviewInputTokenBudget, a.sessionInputTokens)
	}
	if a.sessionInputTokens != 100_000 || result["completed"] != false {
		t.Fatalf("used %d completed %v", a.sessionInputTokens, result["completed"])
	}
}

func TestNoBudgetAttributeLeavesToolLoopUnbounded(t *testing.T) {
	a, ctx, calls := brLoopAgent(t, []brScriptedResponse{{promptTokens: 50_000}, {promptTokens: 50_000}, {promptTokens: 50_000}, {final: true}})
	result, err := a.RunConversation(ctx, "do some tool work", RunConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || result["completed"] != true || result["final_response"] != "done" {
		t.Fatalf("calls %d result %v", calls.Load(), result)
	}
}

func TestReviewInputBudgetExhaustedPredicateEdgeCases(t *testing.T) {
	agent := &AIAgent{}
	if reviewInputBudgetExhausted(agent) {
		t.Fatal("an agent without a budget is never exhausted")
	}
	agent.sessionInputTokens = 999_999
	for _, budget := range []int{0, -1} {
		agent.reviewInputTokenBudget = budget
		if reviewInputBudgetExhausted(agent) {
			t.Fatalf("budget %d armed the gate", budget)
		}
	}
	agent.reviewInputTokenBudget = 100_000
	agent.sessionInputTokens = 99_999
	if reviewInputBudgetExhausted(agent) {
		t.Fatal("under budget reported exhausted")
	}
	agent.sessionInputTokens = 100_000
	if !reviewInputBudgetExhausted(agent) {
		t.Fatal("at budget not reported exhausted")
	}
}

func TestReviewInputTokenBudgetResolution(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cases := []struct {
		value    any
		expected int
	}{{1_000_000, 1_000_000}, {0, 0}, {-5, 0}, {"300000", 300_000}}
	for _, tc := range cases {
		if got := reviewInputTokenBudget(context.Background(), map[string]any{"max_input_tokens": tc.value}, nil); got != tc.expected {
			t.Fatalf("max_input_tokens %v -> %d want %d", tc.value, got, tc.expected)
		}
	}
}

// Context engine answering only its window, the hermes fork(window) namespace
type brWindowEngine struct {
	ContextEngine
	window int
}

func (e brWindowEngine) Base() *ContextEngineBase { return &ContextEngineBase{ContextLength: e.window} }

func TestReviewInputTokenBudgetDefaultTracksForksContextWindow(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fork := func(window int) *AIAgent { return &AIAgent{contextCompressor: brWindowEngine{window: window}} }
	ctx := context.Background()
	if got := reviewInputTokenBudget(ctx, map[string]any{}, fork(65_536)); got != 49_152 {
		t.Fatalf("65k window %d", got)
	}
	if got := reviewInputTokenBudget(ctx, map[string]any{"max_input_tokens": "not-a-number"}, fork(65_536)); got != 49_152 {
		t.Fatalf("malformed %d", got)
	}
	if got := reviewInputTokenBudget(ctx, map[string]any{}, fork(2_000_000)); got != 600_000 {
		t.Fatalf("cap %d", got)
	}
	if got := reviewInputTokenBudget(ctx, map[string]any{}, fork(0)); got != 120_000 {
		t.Fatalf("unknown window %d", got)
	}
	if got := reviewInputTokenBudget(ctx, map[string]any{}, nil); got != 120_000 {
		t.Fatalf("no fork %d", got)
	}
}

func TestBackgroundReviewConfigDoesNotFreezeAFixedInputBudget(t *testing.T) {
	auxiliary, _ := config.Defaults()["auxiliary"].(map[string]any)
	review, ok := auxiliary["background_review"].(map[string]any)
	if !ok {
		t.Fatalf("auxiliary.background_review default %v", auxiliary["background_review"])
	}
	if _, frozen := review["max_input_tokens"]; frozen {
		t.Fatalf("max_input_tokens frozen in the defaults: %v", review)
	}
}
