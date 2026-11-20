package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Loopback agent provider answering one canned completion shaped like what an ACP client returns
func fakeAgentProvider(t *testing.T, projected []any, iterations int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "test-model", "object": "model"}}})
			return
		}
		_, _ = io.ReadAll(r.Body)
		completion := map[string]any{"id": "chatcmpl-acp", "object": "chat.completion", "model": "test-model", "usage": nil,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Edited main.py.", "tool_calls": []any{}, "reasoning": nil}, "finish_reason": "stop"}}}
		if projected != nil {
			completion[ProjectedMessagesAttr] = projected
		}
		if iterations != 0 {
			completion[ProviderToolIterationsAttr] = iterations
		}
		_ = json.NewEncoder(w).Encode(completion)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Drives one real run_conversation turn against the fake agent provider
func runProjectionTurn(t *testing.T, projected []any, iterations int) (*AIAgent, map[string]any) {
	t.Helper()
	srv := fakeAgentProvider(t, projected, iterations)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{Model: "test-model", APIKey: "test-key", BaseURL: srv.URL + "/v1", Platform: "cli", MaxIterations: new(3),
		QuietMode: true, SkipMemory: true, Cwd: h, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.DisableStreaming()
	result, err := a.RunConversation(ctx, "edit main.py", RunConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return a, result
}

func projectionResultMessages(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	switch rows := result["messages"].(type) {
	case []map[string]any:
		return rows
	case []*state.Message:
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, state.MessageToDict(r))
		}
		return out
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.(map[string]any))
		}
		return out
	}
	t.Fatalf("messages %T", result["messages"])
	return nil
}

func rowName(m map[string]any) string {
	name, _ := m["name"].(string)
	return name
}

func TestProviderWorkLandsInTheTranscriptThroughTheRealLoop(t *testing.T) {
	_, result := runProjectionTurn(t, projectedRows(), 1)
	messages := projectionResultMessages(t, result)
	idxResult := -1
	for i, m := range messages {
		if m["role"] == "tool" && rowName(m) == "acpagent_edit" {
			idxResult = i
			break
		}
	}
	if idxResult < 0 {
		t.Fatalf("no projected tool row in %v", messages)
	}
	if content, _ := messages[idxResult]["content"].(string); !strings.Contains(content, "1 file changed") {
		t.Fatalf("tool row content %v", messages[idxResult]["content"])
	}
	idxCall := -1
	for i, m := range messages {
		if calls, ok := m["tool_calls"].([]any); m["role"] == "assistant" && ok && len(calls) > 0 {
			idxCall = i
			break
		}
	}
	if !(idxCall >= 0 && idxCall < idxResult && idxResult < len(messages)-1) {
		t.Fatalf("order call=%d result=%d len=%d", idxCall, idxResult, len(messages))
	}
}

func TestProviderIterationsTickTheSkillNudgeThroughTheRealLoop(t *testing.T) {
	with, _ := runProjectionTurn(t, projectedRows(), 1)
	without, _ := runProjectionTurn(t, nil, 0)
	if with.itersSinceSkill-without.itersSinceSkill != 1 {
		t.Fatalf("iters with %d without %d", with.itersSinceSkill, without.itersSinceSkill)
	}
}

func TestOrdinaryProviderTurnIsUnchanged(t *testing.T) {
	_, result := runProjectionTurn(t, nil, 0)
	for _, m := range projectionResultMessages(t, result) {
		if strings.HasPrefix(rowName(m), "acpagent_") {
			t.Fatalf("ordinary completion gained a projected row %v", m)
		}
	}
}
