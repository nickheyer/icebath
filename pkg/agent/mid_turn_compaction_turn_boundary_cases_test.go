package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Engine whose compression verdict is a test closure and whose usage counters stay as the case set them
type scriptedVerdictEngine struct {
	stubEngine
	shouldCompress func(tokens int) bool
}

func (e *scriptedVerdictEngine) ShouldCompress(promptTokens *int) bool {
	tokens := e.LastPromptTokens
	if promptTokens != nil {
		tokens = *promptTokens
	}
	return e.shouldCompress(tokens)
}

func (e *scriptedVerdictEngine) UpdateFromResponse(map[string]any) {}
func (e *scriptedVerdictEngine) CloneForAgent() ContextEngine      { c := *e; return &c }

// OpenRouter shaped agent over a scripted loopback provider with one web_search tool answering a fixed result
func toolLoopAgent(t *testing.T, script *thinkingScript, maxIterations int, toolResult string) (*AIAgent, context.Context) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	srv := script.server(t)
	env := &tools.Env{Home: h, Cwd: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{Model: "anthropic/claude-sonnet-4-20250514", Provider: "openrouter", APIMode: "chat_completions",
		APIKey: "test-key-1234567890", BaseURL: srv.URL + "/v1", MaxIterations: new(maxIterations), QuietMode: true, SkipMemory: true,
		SkipContextFiles: true, Cwd: h, Env: env, EnabledToolsets: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); provider.WaitReasoningCapsWarms() })
	a.setBaseURL("https://openrouter.ai/api/v1")
	a.DisableStreaming()
	a.cachedSystemPrompt, a.usePromptCaching, a.saveTrajectories = "You are helpful.", false, false
	schema := map[string]any{"name": "web_search", "description": "s",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}}
	reg := tools.NewRegistry()
	reg.Register(tools.Def{Name: "web_search", Toolset: "web", Schema: schema, Handler: func(context.Context, *tools.Call) string { return toolResult }})
	a.toolDispatcher = tools.NewDispatcher(reg)
	a.tools = []any{map[string]any{"type": "function", "function": schema}}
	a.validToolNames = map[string]bool{"web_search": true}
	return a, ctx
}

func patchFacadeCompress(t *testing.T, fn func(a *AIAgent, messages []map[string]any) ([]map[string]any, string)) {
	prev := facadeCompressContext
	facadeCompressContext = func(_ context.Context, a *AIAgent, messages []map[string]any, _ *string, _ compressContextOptions) ([]map[string]any, string, error) {
		out, system := fn(a, messages)
		return out, system, nil
	}
	t.Cleanup(func() { facadeCompressContext = prev })
}

func TestPostToolCompressionReanchorsTheActiveUserBoundary(t *testing.T) {
	caseHome(t)
	compressed := []map[string]any{
		{"role": "user", "content": "compressed history"},
		{"role": "user", "content": "current ask"},
		{"role": "assistant", "tool_calls": []any{map[string]any{"id": "2"}}},
		{"role": "tool", "content": "fresh result", "tool_call_id": "2"},
	}
	engine := &scriptedVerdictEngine{stubEngine: *newStubEngine(0, 0), shouldCompress: func(int) bool { return true }}
	engine.LastPromptTokens, engine.ThresholdTokens = 100, 50
	stale := 4
	a := &AIAgent{contextCompressor: engine, compressionEnabled: true, persistUserMessageIdx: &stale, quietMode: true}
	patchFacadeCompress(t, func(*AIAgent, []map[string]any) ([]map[string]any, string) { return compressed, "system" })
	system := "system"
	s := &LoopState{Messages: rowsToMessages([]map[string]any{{"role": "user", "content": "current ask"}}), SystemMessage: &system,
		UserMessage: "current ask", ActiveSystemPrompt: "system", MaxCompressionAttempts: 1, EffectiveTaskID: "task", FinalResponse: ""}
	endTurn, err := compressAfterToolResults(context.Background(), a, s)
	if err != nil || endTurn {
		t.Fatalf("end turn %v err %v", endTurn, err)
	}
	got := messagesToRows(s.Messages)
	if len(got) != len(compressed) || got[1]["content"] != "current ask" || got[3]["content"] != "fresh result" {
		t.Fatalf("messages %v", got)
	}
	if s.CurrentTurnUserIdx != 1 || a.persistUserMessageIdx == nil || *a.persistUserMessageIdx != 1 {
		t.Fatalf("turn index %d persist anchor %v", s.CurrentTurnUserIdx, a.persistUserMessageIdx)
	}
}

func TestPreApiCompressionMidTurnKeepsThisTurnsToolPairOnTheWire(t *testing.T) {
	toolCall := map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "web_search", "arguments": `{"query": "x"}`}}
	script := &thinkingScript{responses: []map[string]any{
		{"model": "test/model", "usage": nil, "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{toolCall}}, "finish_reason": "tool_calls"}}},
		thinkingMockResponse("done", "stop"),
	}}
	a, ctx := toolLoopAgent(t, script, 6, "RESULT-1")
	var compactions []int
	toolRan := func() bool {
		for _, body := range script.calls() {
			_ = body
		}
		return len(script.calls()) >= 1
	}
	engine := &scriptedVerdictEngine{stubEngine: *newStubEngine(1_000, 0)}
	engine.ThresholdTokens, engine.ProtectFirstN, engine.ProtectLastN, engine.LastPromptTokens = 100, 3, 20, -1
	engine.shouldCompress = func(tokens int) bool { return tokens > 0 && len(compactions) == 0 && toolRan() }
	a.compressionEnabled, a.contextCompressor = true, engine
	patchFacadeCompress(t, func(_ *AIAgent, messages []map[string]any) ([]map[string]any, string) {
		compactions = append(compactions, len(messages))
		return append([]map[string]any{}, messages[2:]...), "compressed prompt"
	})
	var history []map[string]any
	for i := range 30 {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		history = append(history, map[string]any{"role": role, "content": fmt.Sprintf("msg %d", i)})
	}
	result, err := a.RunConversation(ctx, "do tool work", RunConversationOptions{ConversationHistory: history})
	if err != nil {
		t.Fatal(err)
	}
	if result["final_response"] != "done" || len(compactions) != 1 {
		t.Fatalf("final %v compactions %v error %v", result["final_response"], compactions, result["error"])
	}
	calls := script.calls()
	sent, _ := calls[len(calls)-1]["messages"].([]any)
	if len(sent) < 3 {
		t.Fatalf("last request carried %d messages", len(sent))
	}
	var tail [][2]any
	for _, row := range sent[len(sent)-3:] {
		m := row.(map[string]any)
		tail = append(tail, [2]any{m["role"], m["tool_call_id"]})
	}
	if !reflect.DeepEqual(tail, [][2]any{{"user", nil}, {"assistant", nil}, {"tool", "call_1"}}) {
		raw, _ := json.Marshal(sent[len(sent)-3:])
		t.Fatalf("request tail %s", raw)
	}
	assistant, last := sent[len(sent)-2].(map[string]any), sent[len(sent)-1].(map[string]any)
	calls2, _ := assistant["tool_calls"].([]any)
	if len(calls2) != 1 || calls2[0].(map[string]any)["id"] != "call_1" || last["content"] != "RESULT-1" {
		t.Fatalf("assistant %v tool result %v", assistant, last)
	}
}
