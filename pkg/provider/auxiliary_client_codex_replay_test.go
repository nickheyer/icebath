package provider

import (
	"context"
	"testing"
)

// Replayed history whose assistant turn carries a connection scoped Responses message item
func auxCodexReplayMessages() []any {
	return []any{
		map[string]any{"role": "system", "content": "You are helpful."},
		map[string]any{"role": "assistant", "content": "pong", "codex_message_items": []any{map[string]any{
			"type": "message", "role": "assistant", "status": "in_progress",
			"content": []any{map[string]any{"type": "output_text", "text": "pong"}},
			"id":      "msg_short_but_connection_scoped", "phase": "final_answer",
		}}},
		map[string]any{"role": "user", "content": "continue"},
	}
}

// First replayed message item of the captured wire input
func auxCodexCapturedMessageItem(t *testing.T, captured map[string]any) map[string]any {
	t.Helper()
	for _, item := range auxKwargMaps(captured["input"]) {
		if item["type"] == "message" {
			return item
		}
	}
	t.Fatalf("no message item in %v", captured["input"])
	return nil
}

func TestCodexAdapterGithubResponsesMessageIdDrop(t *testing.T) {
	ctx := context.Background()
	t.Run("test_drops_message_id_for_connection_scoped_id_host", func(t *testing.T) {
		registerTestProfile(t, Profile{Name: "acme-replay", Hostnames: []string{"api.acme-replay.test"}, ResponsesDropReplayItemIDs: true})
		adapter, client := newAuxCodexTestAdapter("https://api.acme-replay.test", "gpt-5.5")
		if _, err := adapter.create(ctx, map[string]any{"messages": auxCodexReplayMessages()}); err != nil {
			t.Fatal(err)
		}
		item := auxCodexCapturedMessageItem(t, client.captured)
		if _, has := item["id"]; has || item["phase"] != "final_answer" || item["status"] != "in_progress" {
			t.Fatalf("message item %v", item)
		}
		sameJSON(t, item["content"], []map[string]any{{"type": "output_text", "text": "pong"}}, "content")
	})
	t.Run("test_keeps_message_id_for_codex_backend_host", func(t *testing.T) {
		adapter, client := newAuxCodexTestAdapter("https://chatgpt.com/backend-api/codex", "gpt-5.5")
		if _, err := adapter.create(ctx, map[string]any{"messages": auxCodexReplayMessages()}); err != nil {
			t.Fatal(err)
		}
		if item := auxCodexCapturedMessageItem(t, client.captured); item["id"] != "msg_short_but_connection_scoped" {
			t.Fatalf("message item %v", item)
		}
	})
}
