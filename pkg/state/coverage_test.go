package state

import (
	"context"
	"slices"
	"testing"
)

type covRow struct {
	role, content string
	fields        map[string]any
}

func covCall(id string) map[string]any {
	return map[string]any{"tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}}
}

func TestRepairedReloadCommitArchivesOnlyHeldRows(t *testing.T) {
	const summary = "summary of the held rows"
	unseen := []covRow{{"user", "UNSEEN_USER", nil}, {"assistant", "UNSEEN_REPLY", nil}}
	killed := map[string]any{"tool_call_id": "killed"}
	cases := map[string]struct {
		unseen, suffix []covRow
		render         bool
		late           []covRow
		expected       []any
		tailCount      int
	}{
		"timestamp_rendered_merged_user_names_its_run": {unseen, []covRow{{"user", "U2", nil}, {"user", "U3", nil}, {"assistant", "A3", nil}}, true, nil,
			[]any{summary, "UNSEEN_USER", "UNSEEN_REPLY"}, 0},
		"dropped_row_is_named_not_a_watermark_fallback": {unseen, []covRow{{"user", "U2", nil}, {"assistant", "A2", nil}, {"tool", "ORPHAN", killed}, {"user", "U3", nil}, {"assistant", "A3", nil}}, false, nil,
			[]any{summary, "UNSEEN_USER", "UNSEEN_REPLY"}, 0},
		"composed_drops_record_the_original_row_once": {unseen, []covRow{{"user", "U2", nil}, {"assistant", "tool setup", covCall("z")}, {"tool", "answer z", map[string]any{"tool_call_id": "z"}},
			{"assistant", "", covCall("a")}, {"assistant", "", covCall("b")}, {"user", "U3", nil}, {"assistant", "A3", nil}}, false, nil,
			[]any{summary, "UNSEEN_USER", "UNSEEN_REPLY"}, 0},
		"users_adjacent_only_after_a_drop_skip_the_dropped_row": {unseen, []covRow{{"user", "U2", nil}, {"tool", "orphan", map[string]any{"tool_call_id": "orphan"}}, {"user", "U3", nil}, {"assistant", "A3", nil}}, false, nil,
			[]any{summary, "UNSEEN_USER", "UNSEEN_REPLY"}, 0},
		"folded_assistant_text_never_claims_a_later_row": {nil, []covRow{{"user", "U2", nil}, {"assistant", "A2", nil}, {"assistant", "A2b", nil}, {"user", "U3", nil}, {"assistant", "A3", nil}}, false,
			[]covRow{{"user", "UNSEEN_USER", nil}, {"assistant", "A2\nA2b", nil}}, []any{summary, "UNSEEN_USER", "A2\nA2b"}, 0},
		"watermark_tail_rewind_widens_by_rows_dropped_behind_a_carried_dict": {nil, []covRow{{"user", "U2", nil}, {"assistant", "A2", nil}, {"tool", "ORPHAN", killed}, {"tool", "ORPHAN", killed}, {"user", "U3", nil}, {"assistant", "A3", nil}}, false, nil,
			[]any{summary, "A2", "U3", "A3"}, 3},
	}
	appendRow := func(t *testing.T, db *SessionDB, r covRow) {
		fields := map[string]any{"role": r.role, "content": r.content}
		for k, v := range r.fields {
			fields[k] = v
		}
		readAppend(t, db, "sid", fields)
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := usageDB(t)
			readSession(t, db, NewSession{ID: "sid", Source: "test"})
			appendRow(t, db, covRow{"user", "U1", nil})
			appendRow(t, db, covRow{"assistant", "A1", nil})
			held := archiveConversation(t, db, "sid", true)
			for _, r := range append(append([]covRow{}, c.unseen...), c.suffix...) {
				appendRow(t, db, r)
			}
			held = append(held, archiveConversation(t, db, "sid", false)[2+len(c.unseen):]...)
			watermark := archiveWatermark(t, db, "sid")
			if _, err := RepairMessageSequence(&held); err != nil {
				t.Fatal(err)
			}
			if c.render {
				for _, m := range held[2:] {
					if text, ok := m["content"].(string); ok && m["role"] == "user" {
						m["content"] = "[Fri 2026-11-06 12:00:00 UTC] " + text
					}
				}
			}
			for _, r := range c.late {
				appendRow(t, db, r)
			}
			covered, unresolved, ok, err := CoverageForCommit(db, "sid", held, nil)
			if err != nil {
				t.Fatal(err)
			}
			o := ArchiveCompactOptions{Watermark: watermark, TailCount: c.tailCount, UnresolvedHeld: unresolved}
			if ok {
				o.CoveredIDs = append([]int64{}, covered...)
			}
			compacted := append([]map[string]any{{"role": "user", "content": summary}}, held[len(held)-c.tailCount:]...)
			archiveCompact(t, db, "sid", compacted, o)
			rows, err := db.GetMessages(ctx, "sid", GetMessagesOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var live []any
			for _, r := range rows {
				live = append(live, r.Vals["content"])
			}
			if !slices.Equal(live, c.expected) {
				t.Fatalf("live %q want %q", live, c.expected)
			}
			recalled, err := db.readAll(ctx, "SELECT content FROM messages WHERE session_id = 'sid' AND (active = 1 OR compacted = 1)")
			if err != nil {
				t.Fatal(err)
			}
			var texts []any
			for _, r := range recalled {
				texts = append(texts, r.Vals["content"])
			}
			for _, content := range live {
				if countOf(texts, content) != 1 {
					t.Fatalf("%q recalled %d times", content, countOf(texts, content))
				}
			}
		})
	}
}

func TestCoverageHelpers(t *testing.T) {
	if !holdsAsBlock("steer", "[prior]\n  steer  \n\nsummary") || holdsAsBlock("fix bug", "fix bug in parser") || holdsAsBlock("", "x") {
		t.Fatal("whole line containment")
	}
	img := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:x"}}
	txt := map[string]any{"type": "text", "text": "[Attached image]"}
	if !imagesReplaced([]any{img, txt}, []any{txt, txt}) || imagesReplaced([]any{txt}, []any{txt}) || imagesReplaced([]any{txt}, []any{img}) {
		t.Fatal("image swap detection")
	}
	if got := coverageTextOf([]any{map[string]any{"type": "input_text", "text": "a"}, img, map[string]any{"type": "text", "text": nil}}); got != "a\n" {
		t.Fatalf("text of %q", got)
	}
	if got := tailOriginals([]int64{1, 2, 3, 4, 5}, 2, map[int64]bool{5: true}); !slices.Equal(got, []int64{3, 4, 5}) {
		t.Fatalf("tail originals %v", got)
	}
	if got := carriedTailSlots(map[int64]int{7: 3, 8: 1}, 4, 2); got != 1 {
		t.Fatalf("slots %d", got)
	}
	if n, err := uncountedMergedRows([]map[string]any{{MergedDurableRows: 3, AbsorbedRowIDs: []any{int64(4)}, UnnamedDurableRows: 2}, {"_row_id": int64(9), MergedDurableRows: 5}}); err != nil || n != 3 {
		t.Fatalf("uncounted %d %v", n, err)
	}
	if got := stripTimestampPrefixes("[Mon 2026-01-05 10:00:00 UTC] [2026-01-05T10:00:00Z] hi"); got != "hi" {
		t.Fatalf("strip %q", got)
	}
}

func TestResolveCarriedRowIDsFallsBackToUniqueTimestamp(t *testing.T) {
	db, sid := archiveDB(t)
	ctx := context.Background()
	archiveBatch(t, db, sid, map[string]any{"role": "user", "content": "dup", "timestamp": 1500.0}, map[string]any{"role": "user", "content": "dup", "timestamp": 1500.0}, map[string]any{"role": "assistant", "content": "solo", "timestamp": 1501.0})
	var got []int64
	err := db.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		got, err = db.resolveCarriedRowIDs(ctx, conn, sid, []map[string]any{{"role": "user", "content": "dup", "timestamp": 1500}, {"role": "assistant", "content": "solo", "timestamp": 1501}, nil, {"role": "assistant", "content": "solo"}})
		return err
	}, nil)
	ids := rewriteIDs(t, db, sid, " AND content = 'solo'")
	if err != nil || !slices.Equal(got, ids) {
		t.Fatalf("resolved %v want %v (%v)", got, ids, err)
	}
}
