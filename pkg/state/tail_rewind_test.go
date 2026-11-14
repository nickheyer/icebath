package state

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func tailSummary() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "[CONTEXT COMPACTION] summary of earlier turns"},
		{"role": "assistant", "content": "Continuing from the summary."},
	}
}

func tailDB(t *testing.T, seed int) *SessionDB {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "sess1", Source: "test"})
	for i := range seed {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		readAppend(t, db, "sess1", map[string]any{"role": role, "content": fmt.Sprintf("turn %d", i)})
	}
	return db
}

func tailCompacted() []map[string]any {
	return append(tailSummary(),
		map[string]any{"role": "user", "content": "turn 4"},
		map[string]any{"role": "assistant", "content": "turn 5"})
}

func tailRecall(t *testing.T, db *SessionDB, query string) []string {
	t.Helper()
	hits, err := db.SearchMessages(context.Background(), query, NewSearchMessagesOptions())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		if s, ok := h.Vals["snippet"].(string); ok {
			out[i] = s
		}
	}
	return out
}

func TestTailOriginalsHiddenFromRecall(t *testing.T) {
	db := tailDB(t, 6)
	compacted := tailCompacted()
	if n := archiveCompact(t, db, "sess1", compacted, ArchiveCompactOptions{TailCount: len(compacted) - 2}); n != 4 {
		t.Fatalf("count = %d", n)
	}
	turn5 := archiveCopies(t, db, "sess1", "turn 5")
	if len(turn5) != 2 {
		t.Fatalf("one archived original plus one live clone, got %+v", turn5)
	}
	var originals, lives []archiveRow
	for _, r := range turn5 {
		if r.active == 0 {
			originals = append(originals, r)
		} else if r.active == 1 {
			lives = append(lives, r)
		}
	}
	if len(originals) != 1 || len(lives) != 1 || originals[0].compacted != 0 {
		t.Fatalf("originals %+v lives %+v", originals, lives)
	}
	snippets := tailRecall(t, db, "turn 5")
	marked, plain := true, true
	for _, s := range snippets {
		if strings.Contains(s, ">>>turn 5<<<") && !strings.Contains(s, "[CONTEXT COMPACTION]") {
			marked = false
		}
		low := strings.ToLower(s)
		if strings.Contains(low, "turn 5") && !strings.Contains(low, "[context compaction]") {
			plain = false
		}
	}
	if !marked && !plain {
		t.Fatalf("tail originals must not be recalled: %q", snippets)
	}
}

func TestNonTailOriginalsStillRecalledAsCompacted(t *testing.T) {
	db := tailDB(t, 6)
	compacted := tailCompacted()
	archiveCompact(t, db, "sess1", compacted, ArchiveCompactOptions{TailCount: len(compacted) - 2})
	turn1 := archiveCopies(t, db, "sess1", "turn 1")
	if len(turn1) == 0 || turn1[0].compacted != 1 || turn1[0].active != 0 {
		t.Fatalf("non tail originals must stay compacted=1, got %+v", turn1)
	}
	found := false
	snippets := tailRecall(t, db, "turn 1")
	for _, s := range snippets {
		if strings.Contains(s, "turn") && strings.Contains(s, "1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("summarized away turns must remain recallable: %q", snippets)
	}
}

func TestExplicitCarriedMessagesLeaveSummarizedToolResultsDiscoverable(t *testing.T) {
	db := tailDB(t, 0)
	readAppend(t, db, "sess1", map[string]any{"role": "user", "content": "question 0"})
	readAppend(t, db, "sess1", map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
		map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}},
	}})
	readAppend(t, db, "sess1", map[string]any{"role": "tool", "content": "tool result that was summarized", "tool_call_id": "call-1", "tool_name": "read_file"})
	readAppend(t, db, "sess1", map[string]any{"role": "user", "content": "question 1"})
	readAppend(t, db, "sess1", map[string]any{"role": "assistant", "content": "answer 1"})
	var carried []map[string]any
	for _, m := range archiveConversation(t, db, "sess1", false) {
		switch m["content"] {
		case "question 0", "question 1", "answer 1":
			if _, has := m["_row_id"]; has {
				t.Fatalf("this regression must exercise the row id less resume fallback: %v", m)
			}
			carried = append(carried, m)
		}
	}
	if len(carried) == 0 {
		t.Fatal("no carried messages")
	}
	archiveCompact(t, db, "sess1", []map[string]any{
		{"role": "user", "content": "question 0"},
		{"role": "assistant", "content": "[CONTEXT COMPACTION] summarized tool exchange"},
		{"role": "user", "content": "question 1"},
		{"role": "assistant", "content": "answer 1"},
	}, ArchiveCompactOptions{CarriedMessages: carried})
	tool := archiveCopies(t, db, "sess1", "tool result that was summarized")
	if len(tool) != 1 || tool[0].active != 0 || tool[0].compacted != 1 {
		t.Fatalf("summarized tool results must stay discoverable as compacted history: %+v", tool)
	}
	var original []archiveRow
	for _, r := range archiveCopies(t, db, "sess1", "question 1") {
		if r.active == 0 {
			original = append(original, r)
		}
	}
	if len(original) != 1 || original[0].compacted != 0 {
		t.Fatalf("only exact carried forward originals should be hidden as superseded duplicates: %+v", original)
	}
}

func TestDefaultZeroTailKeepsArchiveEverything(t *testing.T) {
	db := tailDB(t, 6)
	archiveCompact(t, db, "sess1", tailSummary(), ArchiveCompactOptions{})
	archived := archiveCopies(t, db, "sess1", "turn 5")
	if len(archived) == 0 {
		t.Fatal("turn 5 missing")
	}
	for _, r := range archived {
		if r.compacted != 1 || r.active != 0 {
			t.Fatalf("default must still archive everything as compacted=1: %+v", archived)
		}
	}
}

func TestLiveContextLoadUnaffectedByTailCount(t *testing.T) {
	db := tailDB(t, 6)
	compacted := tailCompacted()
	archiveCompact(t, db, "sess1", compacted, ArchiveCompactOptions{TailCount: len(compacted) - 2})
	rows, err := db.GetMessages(context.Background(), "sess1", GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var live []any
	for _, r := range rows {
		live = append(live, r.Vals["content"])
	}
	want := []any{"[CONTEXT COMPACTION] summary of earlier turns", "Continuing from the summary.", "turn 4", "turn 5"}
	if fmt.Sprint(live) != fmt.Sprint(want) || len(live) != len(want) {
		t.Fatalf("live = %q", live)
	}
}

func TestMessageCountReflectsActiveSetAfterTailCount(t *testing.T) {
	db := tailDB(t, 6)
	compacted := tailCompacted()
	if n := archiveCompact(t, db, "sess1", compacted, ArchiveCompactOptions{TailCount: len(compacted) - 2}); n != 4 {
		t.Fatalf("returned = %d", n)
	}
	row, err := db.GetSession(context.Background(), "sess1")
	if err != nil || row == nil {
		t.Fatalf("session row %v %v", row, err)
	}
	mc, ok := row.Vals["message_count"].(int64)
	if !ok || mc != 4 {
		t.Fatalf("message_count = %#v", row.Vals["message_count"])
	}
}
