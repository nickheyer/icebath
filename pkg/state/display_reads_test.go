package state

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func displayDB(t *testing.T, sid string, history ...map[string]any) *SessionDB {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: sid, Source: "test"})
	if len(history) > 0 {
		archiveBatch(t, db, sid, history...)
	}
	return db
}

func displayConversation(t *testing.T, db *SessionDB, sid string, o ConversationOptions) []map[string]any {
	t.Helper()
	msgs, err := db.GetMessagesAsConversation(context.Background(), sid, o)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func displayPaged(t *testing.T, db *SessionDB, sid string) []map[string]any {
	t.Helper()
	rows, err := db.GetMessages(context.Background(), sid, GetMessagesOptions{IncludeCompacted: true})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		out[i] = r.Vals
	}
	return out
}

func displayResumed(t *testing.T, db *SessionDB, sid string) []map[string]any {
	t.Helper()
	_, display, err := db.GetResumeConversations(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return display
}

func displayTextCount(msgs []map[string]any, text string) int {
	n := 0
	for _, m := range msgs {
		n += strings.Count(pyStr(m["content"]), text)
	}
	return n
}

func TestSummaryCarrierDoesNotShowItsMessageTwice(t *testing.T) {
	db := displayDB(t, "s",
		map[string]any{"role": "user", "content": "turn one", "timestamp": 1.0},
		map[string]any{"role": "assistant", "content": "REPLY-ONE words", "timestamp": 2.0},
		map[string]any{"role": "user", "content": "turn two", "timestamp": 3.0},
		map[string]any{"role": "assistant", "content": "REPLY-TWO words", "timestamp": 4.0})
	stored := archiveConversation(t, db, "s", true)
	carried := pruneCopy(stored[1], MergedPriorContextHeader+"\nREPLY-ONE words\n\n"+MergedSummaryDelimiter+"\n\n"+SummaryPrefix+"\nsummary of earlier turns\n\n"+SummaryEndMarker)
	carried["_compressed_summary"] = true
	archiveCompact(t, db, "s", append([]map[string]any{stored[0], carried}, stored[2:]...), ArchiveCompactOptions{})
	shown := displayConversation(t, db, "s", ConversationOptions{IncludeAncestors: true, IncludeCompacted: true})
	for _, read := range [][]map[string]any{shown, displayPaged(t, db, "s")} {
		for _, text := range []string{"REPLY-ONE", "turn one", "turn two", "REPLY-TWO"} {
			if displayTextCount(read, text) != 1 {
				t.Fatalf("%s shown %d times in %v", text, displayTextCount(read, text), archiveContents(read))
			}
		}
	}
}

func TestCompactedToolResultPaintsOnceAsItsArchivedOriginal(t *testing.T) {
	ctx := context.Background()
	full := strings.Repeat("FULL ", 1000)
	stub := "[read_file] pruned stub"
	sid := "stub-display"
	db := displayDB(t, sid,
		map[string]any{"role": "user", "content": "read it", "timestamp": 100.0},
		map[string]any{"role": "assistant", "content": "", "timestamp": 101.0, "tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "content": full, "tool_call_id": "call-1", "tool_name": "read_file", "timestamp": 102.0, "display_metadata": map[string]any{"tool_result_metadata": map[string]any{"lines": 1000}}},
		map[string]any{"role": "assistant", "content": "done reading", "timestamp": 103.0})
	tools := func(read []map[string]any) []any {
		var out []any
		for _, m := range read {
			if m["role"] == "tool" {
				out = append(out, m["content"])
			}
		}
		return out
	}
	for generation := 1; generation <= 2; generation++ {
		history := archiveConversation(t, db, sid, true)
		compacted := []map[string]any{{"role": "user", "content": fmt.Sprintf("summary %d", generation), "timestamp": 150.0 + float64(generation)}}
		for _, m := range history {
			if m["role"] == "tool" {
				m["content"] = stub
			}
			if !pyTruthy(m["_compressed_summary"]) {
				compacted = append(compacted, m)
			}
		}
		archiveCompact(t, db, sid, compacted, ArchiveCompactOptions{})
		paged := displayPaged(t, db, sid)
		anchor, _ := exactInt(paged[0]["id"])
		around, err := db.GetSessionMessagesAround(ctx, sid, anchor, 120)
		if err != nil || around == nil {
			t.Fatalf("around %v %v", around, err)
		}
		for i, read := range [][]map[string]any{displayResumed(t, db, sid), paged, displayConversation(t, db, sid, ConversationOptions{IncludeCompacted: true}), timelineMessages(around)} {
			if got := tools(read); len(got) != 1 || got[0] != full {
				t.Fatalf("generation %d read %d painted %d tool bodies", generation, i, len(got))
			}
		}
		if n, err := db.DisplayMessageCount(ctx, sid); err != nil || n != int64(len(paged)) {
			t.Fatalf("display count %d, paged %d, err %v", n, len(paged), err)
		}
		var model []map[string]any
		for _, m := range archiveConversation(t, db, sid, false) {
			if m["role"] == "tool" {
				model = append(model, m)
			}
		}
		meta, err := pyDumps(model[0]["display_metadata"], pyJSONDefault)
		if err != nil || len(model) != 1 || model[0]["content"] != stub || meta != `{"tool_result_metadata": {"lines": 1000}}` {
			t.Fatalf("model tool rows = %v", model)
		}
	}
}

const displayLiveTurns = 200

func displayIdentitySession(t *testing.T, rewind bool) *SessionDB {
	t.Helper()
	var msgs []map[string]any
	for i := range displayLiveTurns {
		msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("U%d", i), "timestamp": int64(1000000 + 2*i)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("A%d", i), "timestamp": int64(1000001 + 2*i)})
	}
	db := displayDB(t, "s", msgs...)
	if rewind {
		archiveBatch(t, db, "s", map[string]any{"role": "user", "content": "GONE", "timestamp": int64(2000000)}, map[string]any{"role": "assistant", "content": "NO", "timestamp": int64(2000001)})
		for _, m := range displayPaged(t, db, "s") {
			if m["content"] == "GONE" {
				if _, err := db.RewindToMessage(context.Background(), "s", m["id"].(int64), RewindOptions{}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return db
}

func displayIdentityReads(t *testing.T, db *SessionDB) []func() []map[string]any {
	return []func() []map[string]any{
		func() []map[string]any {
			return displayConversation(t, db, "s", ConversationOptions{IncludeAncestors: true, IncludeCompacted: true})
		},
		func() []map[string]any { return displayResumed(t, db, "s") },
		func() []map[string]any {
			return displayConversation(t, db, "s", ConversationOptions{IncludeCompacted: true})
		},
	}
}

func displayHashes(read func() []map[string]any) ([]map[string]any, int) {
	calls := 0
	real := displayIdentityOf
	displayIdentityOf = func(key string) []byte {
		calls++
		return real(key)
	}
	defer func() { displayIdentityOf = real }()
	shown := read()
	return shown, calls
}

func TestADisplayReadWithNoRemovedRowHashesNothing(t *testing.T) {
	db := displayIdentitySession(t, false)
	for i, read := range displayIdentityReads(t, db) {
		shown, hashes := displayHashes(read)
		if len(shown) != 2*displayLiveTurns || hashes != 0 {
			t.Fatalf("read %d shown %d hashes %d", i, len(shown), hashes)
		}
	}
}

func TestADisplayReadHashesOnlyRowsAtARemovedRowsTimestamp(t *testing.T) {
	db := displayIdentitySession(t, true)
	row, err := db.readOne(context.Background(), "SELECT COUNT(*) AS n FROM messages WHERE active = 0 AND compacted = 0")
	if err != nil || row.Vals["n"] != int64(2) {
		t.Fatalf("removed = %v err %v", row, err)
	}
	for i, read := range displayIdentityReads(t, db) {
		shown, hashes := displayHashes(read)
		contents := archiveContents(shown)
		if fmt.Sprint(contents[len(contents)-2:]) != fmt.Sprintf("[U%d A%d]", displayLiveTurns-1, displayLiveTurns-1) || countOf(contents, "GONE") != 0 || hashes > 2 {
			t.Fatalf("read %d tail %v hashes %d", i, contents[len(contents)-2:], hashes)
		}
	}
}

func TestPrunedToolPayloadKeepsOneDisplayIdentity(t *testing.T) {
	sid := "pruned-payload"
	long, _ := json.Marshal(map[string]any{"value": strings.Repeat("L", 4000)})
	db := displayDB(t, sid,
		map[string]any{"role": "user", "content": "start", "timestamp": 100.0},
		map[string]any{"role": "assistant", "content": "Earlier progress", "timestamp": 101.0, "tool_calls": []any{map[string]any{"id": "stable-call", "type": "function", "function": map[string]any{"name": "demo_tool", "arguments": string(long)}}}},
		map[string]any{"role": "tool", "content": strings.Repeat("R", 5000), "tool_call_id": "stable-call", "tool_name": "demo_tool", "timestamp": 102.0},
		map[string]any{"role": "assistant", "content": "Later answer", "timestamp": 200.0})
	history := archiveConversation(t, db, sid, true)
	call, _ := dictValue(displaySeq(t, history[1]["tool_calls"])[0])
	fn, _ := dictValue(call["function"])
	fn["arguments"] = `{"value": "short"}`
	call["function"] = fn
	history[1]["tool_calls"] = []any{call}
	history[2]["content"] = "short result"
	archiveCompact(t, db, sid, history, ArchiveCompactOptions{})
	visible := displayConversation(t, db, sid, ConversationOptions{IncludeRowIDs: true, IncludeCompacted: true})
	var assistants, tools []any
	for _, m := range visible {
		switch m["role"] {
		case "assistant":
			assistants = append(assistants, m["content"])
		case "tool":
			tools = append(tools, m["content"])
		}
	}
	if fmt.Sprint(assistants) != "[Earlier progress Later answer]" || countOf(tools, strings.Repeat("R", 5000)) == 0 {
		t.Fatalf("assistants %v, %d tool rows", assistants, len(tools))
	}
}

func displaySeq(t *testing.T, v any) []any {
	t.Helper()
	items, err := seqIter(v)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestDistinctIdlessAssistantCallsAreNotMerged(t *testing.T) {
	sid := "idless-calls"
	var msgs []map[string]any
	for _, q := range []string{"a", "b"} {
		msgs = append(msgs, map[string]any{"role": "assistant", "content": "working", "timestamp": 1700000000.0, "tool_calls": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "demo_tool", "arguments": `{"q": "` + q + `"}`}}}})
	}
	db := displayDB(t, sid, msgs...)
	if n := len(displayPaged(t, db, sid)); n != 2 {
		t.Fatalf("paged = %d", n)
	}
}

func TestDisplayReadsFetchNoBodyOfARemovedCopy(t *testing.T) {
	ctx := context.Background()
	big := strings.Repeat("x", 100000)
	db := displayDB(t, "s", map[string]any{"role": "user", "content": "U1"}, map[string]any{"role": "assistant", "content": "A1"},
		map[string]any{"role": "user", "content": "U2"}, map[string]any{"role": "assistant", "content": big})
	for n := range 6 {
		archiveCompact(t, db, "s", []map[string]any{{"role": "user", "content": fmt.Sprintf("[CONTEXT SUMMARY] round %d", n)},
			{"role": "user", "content": "U2"}, {"role": "assistant", "content": big}}, ArchiveCompactOptions{TailCount: 2})
	}
	row, err := db.readOne(ctx, "SELECT COUNT(*) AS n FROM messages WHERE content = ? AND active = 0 AND compacted = 0", big)
	if err != nil || row.Vals["n"].(int64) < 5 {
		t.Fatalf("removed = %v err %v", row, err)
	}
	removed, err := db.withRemovedIdentities(ctx, nil, []string{"s"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range removed {
		if r.Vals["content"] == big {
			t.Fatalf("removed row %v fetched its body", r.Vals["id"])
		}
	}
	for i, shown := range [][]map[string]any{displayConversation(t, db, "s", ConversationOptions{IncludeAncestors: true, IncludeCompacted: true}), displayResumed(t, db, "s")} {
		if n := countOf(archiveContents(shown), big); n != 1 {
			t.Fatalf("read %d shows the body %d times", i, n)
		}
	}
}

func TestARewoundTurnStaysHiddenWithoutItsBody(t *testing.T) {
	big := strings.Repeat("x", 100000)
	db := displayDB(t, "s", map[string]any{"role": "user", "content": "KEEP"}, map[string]any{"role": "assistant", "content": "OK"},
		map[string]any{"role": "user", "content": "GONE"}, map[string]any{"role": "assistant", "content": big})
	for _, m := range displayPaged(t, db, "s") {
		if m["content"] == "GONE" {
			if _, err := db.RewindToMessage(context.Background(), "s", m["id"].(int64), RewindOptions{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	shown := displayConversation(t, db, "s", ConversationOptions{IncludeAncestors: true, IncludeCompacted: true})
	if fmt.Sprint(archiveContents(shown)) != "[KEEP OK]" {
		t.Fatalf("shown = %v", archiveContents(shown))
	}
}
