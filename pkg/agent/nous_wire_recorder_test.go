package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestRealAgentUsageRecorderCallsTheHookOnce(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("nous:\n  anthropic_wire: auto\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	type hookCall struct {
		n        int
		upstream string
	}
	var calls []hookCall
	prev := maybeSwitchWireAfterFirstResponse
	maybeSwitchWireAfterFirstResponse = func(a *AIAgent, ctx context.Context, response any, n int) bool {
		calls = append(calls, hookCall{n, classifyUpstream(response)})
		return false
	}
	t.Cleanup(func() { maybeSwitchWireAfterFirstResponse = prev })
	env := &tools.Env{Home: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "jwt", BaseURL: "https://inference-api.nousresearch.com/v1", Provider: "nous",
		APIMode: "chat_completions", Model: "anthropic/claude-fable-5.1", SessionID: "t", Platform: "cli", QuietMode: true,
		SkipContextFiles: true, SkipMemory: true, EnabledToolsets: []string{"file"}, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	usage := map[string]any{"prompt_tokens": 100, "completion_tokens": 5, "total_tokens": 105, "prompt_tokens_details": nil, "completion_tokens_details": nil}
	for n := 1; n <= 3; n++ {
		resp := map[string]any{"usage": usage, "id": "gen-1788708403-xXjDsFwabc", "provider": "Anthropic", "model": "anthropic/claude-fable-5.1"}
		a.recordResponseUsage(ctx, resp, []*state.Message{{Role: "user", Content: "hi"}}, n, 0.1, 0, 3)
	}
	if want := []hookCall{{1, "openrouter"}}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("hook calls %v, want %v", calls, want)
	}
}
