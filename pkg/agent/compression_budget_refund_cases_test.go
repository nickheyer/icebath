package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const marathonThreshold = 10_000

// Compressor whose should_compress reflects the passed estimate and whose verdict latches clear on real usage
type coherentEngine struct {
	stubEngine
	verify, awaiting bool
}

func (e *coherentEngine) ShouldCompress(promptTokens *int) bool {
	return promptTokens != nil && *promptTokens >= e.ThresholdTokens
}

func (e *coherentEngine) UpdateFromResponse(usage map[string]any) {
	e.LastPromptTokens = pyInt(usage["prompt_tokens"])
	e.verify, e.awaiting = false, false
}

func (e *coherentEngine) VerifyCompactionClearedThreshold() bool  { return e.verify }
func (e *coherentEngine) AwaitingRealUsageAfterCompression() bool { return e.awaiting }
func (e *coherentEngine) CloneForAgent() ContextEngine            { c := *e; return &c }

func marathonUsage(promptTokens int) any {
	if promptTokens < 0 {
		return nil
	}
	return map[string]any{"prompt_tokens": promptTokens, "completion_tokens": 1, "total_tokens": promptTokens + 1}
}

func marathonToolResponse(i, promptTokens int) map[string]any {
	call := map[string]any{"id": fmt.Sprintf("call_%d", i), "type": "function",
		"function": map[string]any{"name": "web_search", "arguments": fmt.Sprintf(`{"query": "x%d"}`, i)}}
	return map[string]any{"model": "test/model", "usage": marathonUsage(promptTokens), "choices": []any{map[string]any{
		"message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}}, "finish_reason": "tool_calls"}}}
}

// Drives one turn of oversized tool results and returns the result with the row count of every compaction
func runMarathonTurn(t *testing.T, iterations, providerPromptTokens int) (*AIAgent, map[string]any, []int) {
	t.Helper()
	script := &thinkingScript{}
	for i := range iterations {
		script.responses = append(script.responses, marathonToolResponse(i, providerPromptTokens))
	}
	stop := thinkingMockResponse("done", "stop")
	stop["usage"] = marathonUsage(providerPromptTokens)
	script.responses = append(script.responses, stop)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	srv := script.server(t)
	env := &tools.Env{Home: h, Cwd: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{Model: "anthropic/claude-sonnet-4-20250514", Provider: "openrouter", APIMode: "chat_completions",
		APIKey: "test-key-1234567890", BaseURL: srv.URL + "/v1", MaxIterations: new(20), QuietMode: true, SkipMemory: true, SkipContextFiles: true,
		Cwd: h, Env: env, EnabledToolsets: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); provider.WaitReasoningCapsWarms() })
	a.setBaseURL("https://openrouter.ai/api/v1")
	a.DisableStreaming()
	a.cachedSystemPrompt, a.usePromptCaching, a.saveTrajectories, a.compressionEnabled = "You are helpful.", false, false, true
	engine := &coherentEngine{stubEngine: *newStubEngine(200_000, 0)}
	engine.ThresholdTokens, engine.ProtectFirstN, engine.ProtectLastN = marathonThreshold, 3, 20
	a.contextCompressor = engine
	reg := tools.NewRegistry()
	payload := `{"ok": true, "payload": "` + strings.Repeat("x", 60_000) + `"}`
	schema := map[string]any{"name": "web_search", "description": "web_search tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	reg.Register(tools.Def{Name: "web_search", Toolset: "web", Schema: schema, Handler: func(context.Context, *tools.Call) string { return payload }})
	a.toolDispatcher = tools.NewDispatcher(reg)
	a.tools = []any{map[string]any{"type": "function", "function": schema}}
	a.validToolNames = map[string]bool{"web_search": true}
	if a.maxCompressionAttempts != 3 {
		t.Fatalf("default compression attempt cap %d", a.maxCompressionAttempts)
	}
	var compressions []int
	prev := facadeCompressContext
	facadeCompressContext = func(_ context.Context, _ *AIAgent, messages []map[string]any, _ *string, _ compressContextOptions) ([]map[string]any, string, error) {
		compressions = append(compressions, len(messages))
		engine.verify, engine.awaiting = true, true
		compacted := make([]map[string]any, len(messages))
		for i, m := range messages {
			compacted[i] = m
			if content := m["content"]; pyrepr.Truthy(content) && len([]rune(pyrepr.Str(content))) > 5_000 {
				compacted[i] = shallowRow(m)
				compacted[i]["content"] = "[summarized]"
			}
		}
		return compacted, "compressed prompt", nil
	}
	t.Cleanup(func() { facadeCompressContext = prev })
	result, err := a.RunConversation(ctx, "do a lot of tool work", RunConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return a, result, compressions
}

func TestCompressionBudgetRefund(t *testing.T) {
	t.Run("test_marathon_turn_compacts_past_the_per_turn_cap", func(t *testing.T) {
		_, result, compressions := runMarathonTurn(t, 8, marathonThreshold-1)
		if result["completed"] != true || len(compressions) <= 3 {
			t.Fatalf("completed %v with %d compactions for 8 pressure spikes (%v)", result["completed"], len(compressions), result["error"])
		}
	})
	t.Run("test_unverified_or_pressured_compaction_stays_capped", func(t *testing.T) {
		for _, promptTokens := range []int{-1, marathonThreshold} {
			a, result, compressions := runMarathonTurn(t, 8, promptTokens)
			if result["completed"] != true || len(compressions) > a.maxCompressionAttempts {
				t.Fatalf("prompt tokens %d: completed %v with %d compactions (%v)", promptTokens, result["completed"], len(compressions), result["error"])
			}
		}
	})
}
