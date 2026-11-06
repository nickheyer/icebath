package agent

import (
	"context"
	"log/slog"
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
