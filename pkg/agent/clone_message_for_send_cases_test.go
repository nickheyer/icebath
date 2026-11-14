package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestLegacyHiddenPlaceholderWireSubstitution(t *testing.T) {
	t.Run("test_hidden_row_with_tool_calls_or_text_is_not_touched", func(t *testing.T) {
		h := caseHome(t)
		p := newBlockProvider(t)
		a, ctx := blockAgent(t, h, p, nil)
		history := []map[string]any{
			{"role": "user", "content": "start"},
			{"role": "assistant", "content": "visible text", "display_kind": "hidden", "finish_reason": "stop"},
			{"role": "user", "content": "more"},
			{"role": "assistant", "content": "reply", "finish_reason": "stop"},
		}
		if _, err := a.RunConversation(ctx, "next", RunConversationOptions{ConversationHistory: history, TaskID: "t"}); err != nil {
			t.Fatal(err)
		}
		chats := p.chats()
		if len(chats) == 0 {
			t.Fatal("no request reached the provider")
		}
		wire, _ := chats[len(chats)-1]["messages"].([]any)
		var assistants []map[string]any
		for _, m := range wire {
			if row, _ := m.(map[string]any); row["role"] == "assistant" {
				assistants = append(assistants, row)
			}
		}
		if len(assistants) == 0 || assistants[0]["content"] != "visible text" {
			t.Fatalf("wire assistants %v", assistants)
		}
	})
}

func TestMarkedRowReplaysCleanlyAndNeverReachesProvider(t *testing.T) {
	caseHome(t)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sid := "sess-82888-replay"
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "gateway", Model: "test-model"}); err != nil {
		t.Fatal(err)
	}
	marked := map[string]any{"role": "user", "content": "[ASYNC DELEGATION BATCH COMPLETE — 2/2 succeeded]", "display_kind": "internal_notification"}
	if _, err := db.AppendMessage(ctx, sid, marked, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": "assistant", "content": "noted"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	replayed, err := db.GetMessagesAsConversation(ctx, sid, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var users []map[string]any
	for _, m := range replayed {
		if m["role"] == "user" {
			users = append(users, m)
		}
	}
	if len(users) != 1 {
		t.Fatalf("user rows %v", users)
	}
	userRow := users[0]
	content, _ := userRow["content"].(string)
	if userRow["display_kind"] != "internal_notification" || !strings.HasPrefix(content, "[ASYNC DELEGATION BATCH COMPLETE") {
		t.Fatalf("replayed row %v", userRow)
	}
	apiMsg := cloneMessageForSend(userRow).(map[string]any)
	for _, k := range []string{"api_content", "display_kind", "display_metadata", "_row_id"} {
		delete(apiMsg, k)
	}
	if _, ok := apiMsg["display_kind"]; ok {
		t.Fatal("display_kind reached the provider copy")
	}
	if _, ok := apiMsg["display_metadata"]; ok {
		t.Fatal("display_metadata reached the provider copy")
	}
	if apiMsg["role"] != "user" || userRow["display_kind"] != "internal_notification" {
		t.Fatalf("copy %v original %v", apiMsg, userRow)
	}
}
