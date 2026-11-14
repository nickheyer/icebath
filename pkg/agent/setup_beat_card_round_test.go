package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// A setup_beat card the user wrote past ends the turn on the card's result without another model call
func TestACardClosedByAMessageEndsTheTurnWithoutAnotherModelCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "test-model", "object": "model"}}})
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		message := map[string]any{"role": "assistant", "content": "Elite puts a terminal beside the chat."}
		finish := "stop"
		turn, answered := false, false
		for _, m := range req.Messages {
			turn = turn || m["content"] == "[opening]"
			answered = answered || m["role"] == "tool"
		}
		if !turn {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-aux", "object": "chat.completion", "model": "test-model",
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Setup"}, "finish_reason": "stop"}}})
			return
		}
		if calls.Add(1); !answered {
			message = map[string]any{"role": "assistant", "content": "Which one suits you?", "tool_calls": []any{map[string]any{
				"id": "c1", "type": "function", "function": map[string]any{"name": "setup_beat", "arguments": `{"beat": "layout"}`}}}}
			finish = "tool_calls"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-card", "object": "chat.completion", "model": "test-model",
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
	}))
	t.Cleanup(srv.Close)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	env := &tools.Env{Home: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	db, err := state.Open(home.StatePath(h), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.StartOnboarding(ctx, db, "s1", tools.OnboardingFacts{OS: "macos"}); err != nil {
		t.Fatal(err)
	}
	a, err := NewAIAgent(ctx, AIAgentOptions{Model: "test-model", APIKey: "test-key", BaseURL: srv.URL + "/v1", Platform: "desktop",
		MaxIterations: new(4), QuietMode: true, SkipMemory: true, Cwd: h, Env: env, SessionID: "s1", SessionDB: db,
		EnabledToolsets:   []string{"onboarding"},
		SetupBeatCallback: func(payload map[string]any) map[string]any { return map[string]any{"wrote_instead": true} }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.DisableStreaming()
	result, err := a.RunConversation(ctx, "[opening]", RunConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("model called %d times", calls.Load())
	}
	rows := projectionResultMessages(t, result)
	var tool map[string]any
	for _, m := range rows {
		if m["role"] == "tool" {
			tool = m
			break
		}
	}
	var content map[string]any
	if tool == nil || json.Unmarshal([]byte(tool["content"].(string)), &content) != nil || content["wrote_instead"] != true {
		t.Fatalf("tool row %v", tool)
	}
	if result["interrupted"] != true || rows[len(rows)-1]["role"] != "assistant" {
		t.Fatalf("interrupted %v last %v", result["interrupted"], rows[len(rows)-1])
	}
}
