package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const (
	anchorReply    = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx just-delivered verdict report the user is still reading.  "
	anchorSummary  = "[Recent Summary (d0, node 490)]\n\nEarlier turns summarized."
	anchorFollower = "Thanks, one more question."
)

func assertNoSameRoleAdjacency(t *testing.T, messages []map[string]any) {
	t.Helper()
	for i := 1; i < len(messages); i++ {
		if messages[i-1]["role"] == messages[i]["role"] {
			t.Fatalf("same-role adjacency at %d in %v", i, messages)
		}
	}
}

func countAnchorReply(messages []map[string]any) int {
	n := 0
	for _, m := range messages {
		if s, _ := m["content"].(string); m["role"] == "assistant" && strings.TrimSpace(s) == strings.TrimSpace(anchorReply) {
			n++
		}
	}
	return n
}

func anchorToolChain(ids ...string) []map[string]any {
	var rows []map[string]any
	for _, id := range ids {
		rows = append(rows, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "content": "ok", "tool_call_id": id})
	}
	return rows
}

func anchorOriginal(tail ...map[string]any) []map[string]any {
	rows := []map[string]any{{"role": "user", "content": "Audit the auth module."}, {"role": "assistant", "content": "Short ack."}, {"role": "user", "content": "Go deeper, full report."}}
	for i := 0; i < 6; i++ {
		rows = append(rows, map[string]any{"role": "user", "content": "bulk q " + strings.Repeat("q", 80)}, map[string]any{"role": "assistant", "content": "bulk a " + strings.Repeat("a", 80)})
	}
	rows = append(rows, map[string]any{"role": "assistant", "content": anchorReply, "finish_reason": "stop"})
	return append(rows, tail...)
}

func TestNormalizedTwinOfReplyCountsAsPresent(t *testing.T) {
	for name, kept := range map[string]any{"stripped": strings.TrimSpace(anchorReply), "parts_list": []any{map[string]any{"type": "text", "text": anchorReply}}} {
		t.Run(name, func(t *testing.T) {
			original := []map[string]any{
				{"role": "user", "content": "Audit the auth module."},
				{"role": "assistant", "content": "Short ack."},
				{"role": "user", "content": "Go deeper, full report."},
				{"role": "assistant", "content": anchorReply, "finish_reason": "stop"},
				{"role": "user", "content": anchorFollower},
			}
			compressed := []map[string]any{{"role": "user", "content": anchorSummary}, {"role": "assistant", "content": kept}, {"role": "user", "content": anchorFollower}}
			out, inserted := ensureCompressedKeepsLastAssistantReply(context.Background(), original, compressed, "")
			if inserted != nil || len(out) != 3 {
				t.Fatalf("got %v", out)
			}
			assertNoSameRoleAdjacency(t, out)
		})
	}
}

func TestDroppedReplyIsReinsertedBeforeItsFollower(t *testing.T) {
	original := anchorOriginal(map[string]any{"role": "user", "content": anchorFollower})
	fold := []map[string]any{{"role": "user", "content": anchorSummary}, {"role": "user", "content": anchorFollower}}
	out, inserted := ensureCompressedKeepsLastAssistantReply(context.Background(), original, fold, "S")
	if inserted == nil || len(out) != 3 || countAnchorReply(out) != 1 || out[1]["content"] != anchorReply || inserted[dbPersistedMarker] != true || out[2]["content"] != anchorFollower {
		t.Fatalf("got %v", out)
	}
	if len(fold) != 2 {
		t.Fatal("the engine list was mutated")
	}
}

func TestToolChainSlotIsSkipped(t *testing.T) {
	cases := []struct {
		ids    []string
		reason string
	}{
		{[]string{"call_1", "call_2"}, "strict role alternation"},
		{[]string{"call_0", "call_0", "call_0"}, "reuse tool-call ids"},
		{[]string{"call_0|fc_1", "call_0|fc_2", "call_0|fc_3"}, "reuse tool-call ids"},
	}
	for _, tc := range cases {
		t.Run(tc.ids[0]+tc.reason, func(t *testing.T) {
			var logged []string
			prev := slog.Default()
			slog.SetDefault(slog.New(messageCapture{mu: &sync.Mutex{}, msgs: &logged}))
			t.Cleanup(func() { slog.SetDefault(prev) })
			chain := anchorToolChain(tc.ids...)
			original := anchorOriginal(append([]map[string]any{{"role": "user", "content": anchorFollower}}, chain...)...)
			fold := append([]map[string]any{{"role": "user", "content": state.SummaryPrefix + " earlier turns."}}, chain...)
			out, inserted := ensureCompressedKeepsLastAssistantReply(context.Background(), original, fold, "S")
			if inserted != nil || countAnchorReply(out) != 0 || len(out) != len(fold) {
				t.Fatalf("got %v", out)
			}
			found := false
			for _, line := range logged {
				found = found || (strings.Contains(line, "not reinserting") && strings.Contains(line, tc.reason))
			}
			if !found {
				t.Fatalf("no skip log with %q in %v", tc.reason, logged)
			}
		})
	}
}

func TestOlderAssistantAtSlotIsNotDisplaced(t *testing.T) {
	original := anchorOriginal()
	original = append(original[:len(original)-1], map[string]any{"role": "user", "content": "ok"}, map[string]any{"role": "assistant", "content": "noted"},
		map[string]any{"role": "user", "content": "ok"}, original[len(original)-1], map[string]any{"role": "user", "content": "ok"})
	fold := []map[string]any{{"role": "user", "content": anchorSummary}, {"role": "assistant", "content": "noted"}, {"role": "user", "content": "ok"}}
	out, inserted := ensureCompressedKeepsLastAssistantReply(context.Background(), original, fold, "")
	if inserted != nil || countAnchorReply(out) != 0 {
		t.Fatalf("got %v", out)
	}
	assertNoSameRoleAdjacency(t, out)
}

func TestReplyInsertionIndexPlacesBeforeATrailingRealUser(t *testing.T) {
	followers := []map[string]any{{"role": "user", "content": "new ask"}}
	compressed := []map[string]any{{"role": "user", "content": anchorSummary}, {"role": "assistant", "content": "older"}, {"role": "user", "content": "rewritten ask"}}
	if idx, ok := replyInsertionIndex(followers, compressed, idSet{}); !ok || idx != 2 {
		t.Fatalf("got %d %v", idx, ok)
	}
	if idx, ok := replyInsertionIndex(nil, compressed, idSet{}); !ok || idx != 3 {
		t.Fatalf("no followers got %d %v", idx, ok)
	}
}

// Real SessionDB with the persisted history, an engine returning fold, and the live list ending on the unpersisted follower
func seedReplyAnchorAgent(t *testing.T, tailRows [][2]string, fold []map[string]any, follower string) (*state.SessionDB, string, []map[string]any, *AIAgent) {
	t.Helper()
	ctx := context.Background()
	db := newRecoveryDB(t)
	sessionID := "E2E_118900_REPLY"
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	rows := [][2]string{{"user", "Audit the auth module."}, {"assistant", "Short ack."}, {"user", "Go deeper, full report."}}
	for i := 0; i < 6; i++ {
		rows = append(rows, [2]string{"user", fmt.Sprintf("bulk q %d ", i) + strings.Repeat("q", 80)}, [2]string{"assistant", fmt.Sprintf("bulk a %d ", i) + strings.Repeat("a", 80)})
	}
	rows = append(append(rows, tailRows...), [2]string{"assistant", anchorReply})
	for _, r := range rows {
		if _, err := db.AppendMessage(ctx, sessionID, map[string]any{"role": r[0], "content": r[1]}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	persisted, err := db.GetMessagesAsConversation(ctx, sessionID, state.ConversationOptions{IncludeRowIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	messages := append(persisted, map[string]any{"role": "user", "content": follower})
	if _, ok := messages[len(messages)-2]["_row_id"].(int64); !ok || messages[len(messages)-2]["timestamp"] == nil {
		t.Fatalf("live reply row lacks its row id or timestamp: %v", messages[len(messages)-2])
	}
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", Platform: "telegram", SessionDB: db, SessionID: sessionID})
	engine := &scriptedEngine{stubEngine: *newStubEngine(200000, 0.5), compress: func([]map[string]any) []map[string]any { return fold }}
	engine.CompressionCount = 1
	a.contextCompressor = engine
	return db, sessionID, messages, a
}

func compressReplyAnchorAgent(t *testing.T, a *AIAgent, messages []map[string]any) []map[string]any {
	t.Helper()
	idx := len(messages) - 1
	a.persistUserMessageIdx = &idx
	approx := 120000
	system := "sys"
	out, _, err := a.compressContext(context.Background(), messages, &system, compressContextOptions{ApproxTokens: &approx, CommitFence: NewCompressionCommitFence()})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func liveContents(t *testing.T, db *state.SessionDB, sessionID string, o state.ConversationOptions) ([]map[string]any, []any) {
	t.Helper()
	live, err := db.GetMessagesAsConversation(context.Background(), sessionID, o)
	if err != nil {
		t.Fatal(err)
	}
	contents := make([]any, len(live))
	for i, m := range live {
		contents[i] = m["content"]
	}
	return live, contents
}

// Compresses, then checks old rows archived and both views alternating and ending on tailRole
func compressAndReadReplyAnchor(t *testing.T, db *state.SessionDB, sessionID string, a *AIAgent, messages []map[string]any, tailRole string) ([]map[string]any, []any) {
	t.Helper()
	out := compressReplyAnchorAgent(t, a, messages)
	live, contents := liveContents(t, db, sessionID, state.ConversationOptions{})
	if slices.Contains(contents, any("Short ack.")) || slices.Contains(contents, any("Go deeper, full report.")) {
		t.Fatalf("superseded early rows still live: %v", contents)
	}
	for _, view := range [][]map[string]any{out, live} {
		assertNoSameRoleAdjacency(t, view)
		if view[len(view)-1]["role"] != tailRole {
			t.Fatalf("view ends on %v, want %s: %v", view[len(view)-1]["role"], tailRole, view)
		}
	}
	return live, contents
}

func TestReplyRowStaysActiveAfterCommit(t *testing.T) {
	for _, todoStore := range []bool{false, true} {
		t.Run(map[bool]string{false: "dropped", true: "dropped_with_todo_store"}[todoStore], func(t *testing.T) {
			fold := []map[string]any{{"role": "user", "content": anchorSummary}, {"role": "user", "content": anchorFollower}}
			db, sessionID, messages, a := seedReplyAnchorAgent(t, nil, fold, anchorFollower)
			if todoStore {
				if _, err := a.todoStore.Write([]any{map[string]any{"id": "t1", "content": "inspect image", "status": "pending"}}, false); err != nil {
					t.Fatal(err)
				}
			}
			live, contents := compressAndReadReplyAnchor(t, db, sessionID, a, messages, "user")
			if countAnchorReply(live) != 1 {
				t.Fatalf("just-delivered reply left the active set after compaction: %v", contents)
			}
			if !slices.ContainsFunc(contents, func(c any) bool { s, _ := c.(string); return strings.HasPrefix(s, anchorFollower) }) {
				t.Fatalf("follower missing: %v", contents)
			}
			display, _ := liveContents(t, db, sessionID, state.ConversationOptions{IncludeAncestors: true, IncludeRowIDs: true, IncludeCompacted: true})
			if countAnchorReply(display) != 1 {
				t.Fatalf("display renders the reply %d times: %v", countAnchorReply(display), display)
			}
		})
	}
}

func TestToolChainSlotIsSkippedEndToEnd(t *testing.T) {
	cases := []struct {
		name   string
		ids    []string
		reason string
	}{
		{"unique_ids", []string{"call_1", "call_2"}, "strict role alternation"},
		{"constant_ids", []string{"call_0", "call_0", "call_0"}, "reuse tool-call ids"},
		{"composite_ids", []string{"call_0|fc_1", "call_0|fc_2", "call_0|fc_3"}, "reuse tool-call ids"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fold := append([]map[string]any{{"role": "user", "content": state.SummaryPrefix + " earlier turns."}}, anchorToolChain(tc.ids...)...)
			db, sessionID, messages, a := seedReplyAnchorAgent(t, nil, fold, anchorFollower)
			messages = append(messages, fold[1:]...)
			var logged []string
			prev := slog.Default()
			slog.SetDefault(slog.New(messageCapture{mu: &sync.Mutex{}, msgs: &logged}))
			t.Cleanup(func() { slog.SetDefault(prev) })
			live, _ := compressAndReadReplyAnchor(t, db, sessionID, a, messages, "tool")
			slog.SetDefault(prev)
			if countAnchorReply(live) != 0 {
				t.Fatalf("reply recovered into the chain: %v", live)
			}
			calls := 0
			for _, m := range live {
				if m["role"] == "assistant" && m["tool_calls"] == nil {
					t.Fatalf("assistant without tool calls in the chain: %v", live)
				}
				if m["tool_calls"] != nil {
					calls++
				}
			}
			if calls != len(tc.ids) {
				t.Fatalf("tool call rows %d, want %d: %v", calls, len(tc.ids), live)
			}
			if !slices.ContainsFunc(logged, func(l string) bool { return strings.Contains(l, "not reinserting") && strings.Contains(l, tc.reason) }) {
				t.Fatalf("no skip log with %q in %v", tc.reason, logged)
			}
		})
	}
}

func TestOlderAssistantAtSlotIsNotDisplacedEndToEnd(t *testing.T) {
	fold := []map[string]any{{"role": "user", "content": anchorSummary}, {"role": "assistant", "content": "noted"}, {"role": "user", "content": "ok"}}
	db, sessionID, messages, a := seedReplyAnchorAgent(t, [][2]string{{"user", "ok"}, {"assistant", "noted"}, {"user", "ok"}}, fold, "ok")
	out := compressReplyAnchorAgent(t, a, messages)
	live, contents := liveContents(t, db, sessionID, state.ConversationOptions{})
	for _, view := range [][]map[string]any{out, live} {
		assertNoSameRoleAdjacency(t, view)
		if view[len(view)-1]["role"] != "user" {
			t.Fatalf("view ends on %v: %v", view[len(view)-1]["role"], view)
		}
	}
	if countAnchorReply(live) != 0 {
		t.Fatalf("reply forced in beside noted: %v", contents)
	}
	if noted, ok := slices.Index(contents, any("noted")), slices.Index(contents, any("ok")); !(noted >= 0 && noted < len(contents)-1 && len(contents)-1 == ok) {
		t.Fatalf("chronology broken: %v", contents)
	}
}
