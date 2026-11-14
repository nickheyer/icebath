package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Loopback provider whose every answer is interim text plus one web_search call
func interimToolCallServer(t *testing.T, content, callID string) *httptest.Server {
	t.Helper()
	call := map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": "web_search", "arguments": "{}"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "test/model", "object": "model"}}})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			streamed := map[string]any{"index": 0, "id": callID, "type": "function", "function": map[string]any{"name": "web_search", "arguments": "{}"}}
			for _, part := range []struct {
				delta  map[string]any
				finish any
			}{{map[string]any{"role": "assistant", "content": content}, nil}, {map[string]any{"tool_calls": []any{streamed}}, nil}, {map[string]any{}, "tool_calls"}} {
				chunk, _ := json.Marshal(map[string]any{"id": "m", "model": "test/model", "choices": []any{map[string]any{"index": 0, "delta": part.delta, "finish_reason": part.finish}}})
				fmt.Fprintf(w, "data: %s\n\n", chunk)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "m", "model": "test/model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content, "tool_calls": []any{call}}, "finish_reason": "tool_calls"}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Conversation rows a fresh connection reads back from the store file
func durableConversation(t *testing.T, dbPath, sessionID string) []map[string]any {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.GetMessagesAsConversation(context.Background(), sessionID, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func durableRoles(t *testing.T, dbPath, sessionID string) []any {
	var roles []any
	for _, row := range durableConversation(t, dbPath, sessionID) {
		roles = append(roles, row["role"])
	}
	return roles
}

func TestInterimAssistantIsDurableBeforeUIProjectionOnAbnormalExit(t *testing.T) {
	h := blockHome(t)
	srv := interimToolCallServer(t, "I'll inspect the repository now.", "visible-call")
	dbPath := filepath.Join(t.TempDir(), "state.db")
	sessionID := "interim-abnormal-exit"
	db, err := state.NewSessionDB(context.Background(), dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "tui", Model: "test/model"}); err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Home: h, Cwd: h}
	ctx := tools.WithToolEnv(context.Background(), env)
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "test-key", BaseURL: srv.URL + "/v1", Provider: "openai-compat", Model: "test/model",
		EnabledToolsets: []string{}, QuietMode: true, SkipContextFiles: true, SkipMemory: true, SessionDB: db, SessionID: sessionID, Cwd: h, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	a.saveTrajectories, a.compressionEnabled, a.usePromptCaching = false, false, false
	a.cachedSystemPrompt = "You are helpful."
	a.sessionDBCreated = true
	a.tools = []any{map[string]any{"type": "function", "function": map[string]any{"name": "web_search", "description": "web_search tool",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}
	a.validToolNames = map[string]bool{"web_search": true}
	var rolesSeenByUI []any
	exited := false
	a.interimAssistantCallback = func(string, bool) {
		rolesSeenByUI = append(rolesSeenByUI, durableRoles(t, dbPath, sessionID)...)
		exited = true
		runtime.Goexit()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.RunConversation(ctx, "inspect the repository", RunConversationOptions{TaskID: "t"})
	}()
	<-done
	_ = db.Close()
	if !exited {
		t.Fatal("the interim projection never ran")
	}
	if !reflect.DeepEqual(rolesSeenByUI, []any{"user", "assistant"}) {
		t.Fatalf("roles the UI saw %v", rolesSeenByUI)
	}
	durable := durableConversation(t, dbPath, sessionID)
	var roles []any
	for _, m := range durable {
		roles = append(roles, m["role"])
	}
	if !reflect.DeepEqual(roles, []any{"user", "assistant"}) || durable[1]["content"] != "I'll inspect the repository now." {
		t.Fatalf("durable %v", durable)
	}
	calls, _ := durable[1]["tool_calls"].([]any)
	if len(calls) == 0 {
		t.Fatalf("no tool calls stored %v", durable[1])
	}
	if first, _ := calls[0].(map[string]any); first["id"] != "visible-call" {
		t.Fatalf("tool calls %v", durable[1]["tool_calls"])
	}
	resumed := sanitizeAPIMessages(context.Background(), sessionID, durable)
	var resumedRoles []any
	for _, m := range resumed {
		resumedRoles = append(resumedRoles, m["role"])
	}
	if !reflect.DeepEqual(resumedRoles, []any{"user", "assistant", "tool"}) || resumed[2]["tool_call_id"] != "visible-call" {
		t.Fatalf("resumed %v", resumed)
	}
	if n := len(durableConversation(t, dbPath, sessionID)); n != 2 {
		t.Fatalf("reconciliation wrote %d rows", n)
	}
}
