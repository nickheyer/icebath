package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session database holding the given rows, an agent bound to it and the same rows as flushed live messages
func closureFixture(t *testing.T, source, prov, model string, rows []map[string]any) (*state.SessionDB, *AIAgent, []*state.Message) {
	t.Helper()
	caseHome(t)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: source, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["stored"] == false {
			continue
		}
		if _, err := db.AppendMessage(ctx, "s1", row, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	live := make([]map[string]any, len(rows))
	for i, row := range rows {
		live[i] = map[string]any{}
		for k, v := range row {
			if k != "stored" {
				live[i][k] = v
			}
		}
	}
	messages := rowsToMessages(live)
	for _, m := range messages {
		m.Persisted = true
	}
	a := spAgent(t)
	a.sessionDB, a.sessionID, a.provider, a.model = db, "s1", prov, model
	return db, a, messages
}

func closureStored(t *testing.T, db *state.SessionDB) []map[string]any {
	t.Helper()
	rows, err := db.GetMessagesAsConversation(context.Background(), "s1", state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func closureSurfaceCode(row map[string]any) any {
	raw, err := pyjson.Dumps(row["display_metadata"])
	if err != nil {
		return err
	}
	var meta struct {
		Surface map[string]any `json:"error_surface"`
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return err
	}
	return meta.Surface["code"]
}

func TestFailedTurnBoundaryIsIdempotentOnTheDurableTailAndSkipsContextOverflow(t *testing.T) {
	call := map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}}
	db, a, messages := closureFixture(t, "acp", "", "", []map[string]any{
		{"role": "user", "content": "delete the build directory"},
		{"role": "assistant", "content": nil, "tool_calls": []any{call}, "stored": false},
		{"role": "tool", "tool_call_id": "t1", "content": "removed", "stored": false},
	})
	ctx := context.Background()
	closeDurableFailedTurn(ctx, a, map[string]any{"completed": false, "failed": true, "failure_reason": "context_overflow",
		"messages": append([]*state.Message(nil), messages...)})
	if role, _ := db.LatestConversationRole(ctx, "s1"); len(closureStored(t, db)) != 1 || role != "user" {
		t.Fatalf("context overflow wrote a boundary, tail %q", role)
	}
	failed := map[string]any{"completed": false, "failed": true, "failure_reason": "server_error", "messages": messages, "current_turn_user_idx": 0}
	closeDurableFailedTurn(ctx, a, failed)
	closeDurableFailedTurn(ctx, a, failed)
	if stored := closureStored(t, db); len(stored) != 2 {
		t.Fatalf("stored rows %v", stored)
	}
	out := failed["messages"].([]*state.Message)
	if last := out[len(out)-1]; last.Role != "assistant" || last.Content != PartialFailedTurnNotice {
		t.Fatalf("boundary %+v", last)
	}
	if role, _ := db.LatestConversationRole(ctx, "s1"); role != "assistant" {
		t.Fatalf("tail %q", role)
	}
}

func TestFailedTurnBoundaryKeepsTheErrorCardForRehydration(t *testing.T) {
	db, a, messages := closureFixture(t, "acp", "opencode-go", "deepseek-v4.1-flash", []map[string]any{{"role": "user", "content": "say hello"}})
	result := map[string]any{"completed": false, "failed": true, "failure_reason": "timeout", "error": "Connection error.", "messages": messages}
	closeDurableFailedTurn(context.Background(), a, result)
	out := result["messages"].([]*state.Message)
	meta := out[len(out)-1].DisplayMetadata
	surface, _ := meta["error_surface"].(map[string]any)
	if meta["error"] != "Connection error." || surface["code"] != "timeout" || surface["provider"] != "opencode-go" {
		t.Fatalf("metadata %v", meta)
	}
	stored := closureStored(t, db)
	if code := closureSurfaceCode(stored[len(stored)-1]); code != "timeout" {
		t.Fatalf("stored surface code %v", code)
	}
}

func TestFailedTurnAfterAToolRowIsClosedWithItsErrorCard(t *testing.T) {
	call := map[string]any{"id": "call_setup_beat_name", "type": "function",
		"function": map[string]any{"name": "setup_beat", "arguments": `{"beat":"name"}`}}
	db, a, messages := closureFixture(t, "tui", "nous", "m", []map[string]any{
		{"role": "user", "content": "opening"},
		{"role": "assistant", "content": "Hi!", "tool_calls": []any{call}},
		{"role": "tool", "tool_call_id": "call_setup_beat_name", "content": `{"answer":{"name":"Austin"}}`},
	})
	failed := map[string]any{"completed": false, "failed": true, "failure_reason": "rate_limit", "error": "429 Too Many Requests",
		"messages": messages, "current_turn_user_idx": 0}
	closeDurableFailedTurn(context.Background(), a, failed)
	closeDurableFailedTurn(context.Background(), a, failed)
	stored := closureStored(t, db)
	var roles []string
	for _, row := range stored {
		roles = append(roles, row["role"].(string))
	}
	if len(roles) != 4 || roles[0] != "user" || roles[1] != "assistant" || roles[2] != "tool" || roles[3] != "assistant" {
		t.Fatalf("roles %v", roles)
	}
	boundary := stored[3]
	if boundary["content"] != PartialFailedTurnNotice || boundary["display_kind"] != FailedTurnDisplayKind {
		t.Fatalf("boundary %v", boundary)
	}
	if code := closureSurfaceCode(boundary); code != "rate_limit" {
		t.Fatalf("surface code %v", code)
	}
}
