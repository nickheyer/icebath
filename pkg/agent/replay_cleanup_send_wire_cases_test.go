package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Chat completions wire bytes of messages through the real transport, keys sorted
func sendWire(t *testing.T, messages []map[string]any) string {
	t.Helper()
	converted, err := provider.GetTransport("chat_completions").ConvertMessages(messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(converted)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Request rows the live send path builds for history with the current turn at idx
func sendRequest(ctx context.Context, a *AIAgent, history []map[string]any, idx int) []map[string]any {
	request, _ := buildAPIMessages(ctx, a, rowsToMessages(history), idx, "", "", nil, "")
	return request
}

func sendAgent(env *tools.Env) *AIAgent {
	return &AIAgent{apiMode: "chat_completions", currentTurnTimestamp: 10_000.0, env: env}
}

func wireRows(t *testing.T, rows []map[string]any) []map[string]any {
	t.Helper()
	raw, _ := json.Marshal(rows)
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReplayCleanupSendWire(t *testing.T) {
	caseHome(t)
	t.Run("test_send_wire_matches_replay_wire_after_db_round_trip", func(t *testing.T) {
		h := caseHome(t)
		env := &tools.Env{Home: h}
		ctx := tools.WithToolEnv(context.Background(), env)
		now := 10_000.0
		db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "t.db"), false)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli"}); err != nil {
			t.Fatal(err)
		}
		call := func(id, name string) map[string]any {
			return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": "{}"}}
		}
		grepHit, _ := json.Marshal(map[string]any{"output": "docs.txt:12: [execution interrupted — user stop]\nloop.py:4: [Command interrupted]", "exit_code": 0})
		docText := "how interrupts render:\n[Command interrupted]\n(the marker above is documentation)"
		for _, row := range []map[string]any{
			{"role": "user", "content": "hello", "api_content": "hello [with memory]", "timestamp": now - 300},
			{"role": "assistant", "content": "hi", "timestamp": now - 299},
			{"role": "user", "content": "confirm reboot", "timestamp": now - 120},
			{"role": "assistant", "content": "", "tool_calls": []any{call("c1", "read_file")}, "timestamp": now - 119},
			{"role": "tool", "content": `{"output": "[execution interrupted — user stop]", "exit_code": -1}`, "tool_call_id": "c1", "tool_name": "read_file", "timestamp": now - 118},
			{"role": "assistant", "content": "", "tool_calls": []any{call("c2", "search_files"), call("c3", "read_file")}, "timestamp": now - 110},
			{"role": "tool", "content": string(grepHit), "tool_call_id": "c2", "tool_name": "search_files", "timestamp": now - 109},
			{"role": "tool", "content": docText, "tool_call_id": "c3", "tool_name": "read_file", "timestamp": now - 108},
		} {
			if _, err := db.AppendMessage(ctx, "s1", row, state.AppendGuards{}); err != nil {
				t.Fatal(err)
			}
		}
		persisted, err := db.GetMessagesAsConversation(ctx, "s1", state.ConversationOptions{})
		_ = db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if persisted[0]["api_content"] != "hello [with memory]" || persisted[2]["timestamp"] == nil {
			t.Fatalf("persisted %v", persisted[:3])
		}
		var replay []map[string]any
		for _, m := range CanonicalizeReplayHistory(wireRows(t, persisted), &now) {
			row := map[string]any{}
			for k, v := range m {
				row[k] = v
			}
			if s, _ := m["api_content"].(string); s != "" {
				row["content"] = s
			}
			replay = append(replay, row)
		}
		live := append(wireRows(t, persisted), map[string]any{"role": "user", "content": "now", "timestamp": now})
		frozen := wireRows(t, live)
		request := sendRequest(ctx, sendAgent(env), live, len(live)-1)
		if !reflect.DeepEqual(wireRows(t, live), frozen) {
			t.Fatal("send path mutated the live history")
		}
		if sendWire(t, request) != sendWire(t, append(replay, map[string]any{"role": "user", "content": "now"})) {
			t.Fatalf("send wire %s\nreplay wire %s", sendWire(t, request), sendWire(t, replay))
		}
		first, _ := request[0]["content"].(string)
		third, _ := request[2]["content"].(string)
		if !strings.Contains(first, "[with memory]") || !strings.Contains(third, "EXPIRED") {
			t.Fatalf("first %q third %q", first, third)
		}
		var roles []any
		for _, m := range request {
			roles = append(roles, m["role"])
		}
		if !reflect.DeepEqual(roles, []any{"user", "assistant", "user", "assistant", "tool", "tool", "user"}) {
			t.Fatalf("roles %v", roles)
		}
		if request[4]["content"] != string(grepHit) || request[5]["content"] != docText {
			t.Fatalf("tool rows %q %q", request[4]["content"], request[5]["content"])
		}
		live = append(live, map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call("c4", "search_files")}},
			map[string]any{"role": "tool", "tool_call_id": "c4", "content": "[Command interrupted]"})
		request2 := sendRequest(ctx, sendAgent(env), live, len(persisted))
		if sendWire(t, request2[:len(request)]) != sendWire(t, request) || request2[len(request2)-1]["content"] != "[Command interrupted]" {
			t.Fatalf("second request %v", request2)
		}
	})
	t.Run("test_confirmation_expiry_uses_frozen_admission_clock", func(t *testing.T) {
		h := caseHome(t)
		env := &tools.Env{Home: h}
		ctx := tools.WithToolEnv(context.Background(), env)
		a := sendAgent(env)
		a.toolGuardrails, a.memoryStore, a.maxIterations = tools.NewToolCallGuardrailController(nil), nil, 4
		history := []map[string]any{
			{"role": "user", "content": "confirm reboot", "timestamp": 9_941.0},
			{"role": "assistant", "content": "ok"},
			{"role": "user", "content": "go", "timestamp": 10_000.0},
		}
		prev := loopNow
		loopNow = func() float64 { return 10_070.0 }
		t.Cleanup(func() { loopNow = prev })
		resetPerTurnAgentState(ctx, a)
		request := sendRequest(ctx, a, history, len(history)-1)
		if first, _ := request[0]["content"].(string); !strings.Contains(first, "EXPIRED") {
			t.Fatalf("first %q", first)
		}
		at := 10_070.0
		if sendWire(t, request[:2]) != sendWire(t, CanonicalizeReplayHistory(wireRows(t, history[:2]), &at)) {
			t.Fatalf("wire %s", sendWire(t, request[:2]))
		}
	})
}
