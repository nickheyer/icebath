package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func pruneAssistantCall(callID string) map[string]any {
	return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
		"id": callID, "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"cmd":"ls"}`}}}}
}

func pruneHistory() []map[string]any {
	msgs := []map[string]any{{"role": "user", "content": "start"}}
	for i := range 8 {
		callID := fmt.Sprintf("call_%d", i)
		content := "ok"
		if i < 3 {
			content = strings.Repeat(string(rune(65+i)), 24000)
		}
		msgs = append(msgs, pruneAssistantCall(callID), map[string]any{"role": "tool", "tool_call_id": callID, "content": content})
	}
	return msgs
}

func pruneDB(t *testing.T, sid string, history []map[string]any) *SessionDB {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: sid, Source: "telegram"})
	archiveBatch(t, db, sid, history...)
	return db
}

type pruneRow struct {
	id                int64
	role              any
	content           any
	active, compacted int64
}

func pruneRows(t *testing.T, db *SessionDB, sid string) []pruneRow {
	t.Helper()
	rows, err := db.readAll(context.Background(), "SELECT id, role, content, active, compacted FROM messages WHERE session_id = ? ORDER BY id", sid)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]pruneRow, len(rows))
	for i, r := range rows {
		out[i] = pruneRow{r.Vals["id"].(int64), r.Vals["role"], r.Vals["content"], r.Vals["active"].(int64), r.Vals["compacted"].(int64)}
	}
	return out
}

func pruneCopy(m map[string]any, content any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	if content != nil {
		out["content"] = content
	}
	return out
}

// Rewrites every large tool result of the conversation to a short marker through the in place writer
func pruneLargeResults(t *testing.T, db *SessionDB, sid string, msgs []map[string]any) []map[string]any {
	t.Helper()
	pruned := make([]map[string]any, len(msgs))
	var changes []PruneChange
	for i, m := range msgs {
		pruned[i] = m
		if s, ok := m["content"].(string); ok && m["role"] == "tool" && len(s) > 8000 {
			pruned[i] = pruneCopy(m, "[pruned "+pyStr(m["tool_call_id"])+"]")
			changes = append(changes, PruneChange{pruneCopy(m, nil), pruned[i]})
		}
	}
	n, err := db.RewritePrunedRows(context.Background(), sid, changes, PruneRewriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n != len(changes) || n == 0 {
		t.Fatalf("rewrote %d of %d", n, len(changes))
	}
	return pruned
}

func prunePage(t *testing.T, db *SessionDB, sid string) []any {
	return archiveDisplayContents(t, db, sid)
}

func TestRewritePrunedRowsRefusesUserRows(t *testing.T) {
	sid := "PRUNE_IN_PLACE_USER"
	db := pruneDB(t, sid, pruneHistory())
	msgs := archiveConversation(t, db, sid, false)
	before := pruneRows(t, db, sid)
	_, err := db.RewritePrunedRows(context.Background(), sid, []PruneChange{{msgs[0], pruneCopy(msgs[0], "rewritten")}}, PruneRewriteOptions{})
	var unresolved *PruneRowUnresolvedError
	if !errors.As(err, &unresolved) {
		t.Fatalf("err = %v", err)
	}
	if fmt.Sprint(pruneRows(t, db, sid)) != fmt.Sprint(before) {
		t.Fatal("rows changed after a refused prune")
	}
}

func TestAssistantToolCallsRewriteIsStaleInTheRowLocalWriter(t *testing.T) {
	sid := "PRUNE_STALE_TOOL_CALLS"
	db := pruneDB(t, sid, pruneHistory())
	msgs := archiveConversation(t, db, sid, true)
	var held map[string]any
	for _, m := range msgs {
		if m["role"] == "assistant" && pyTruthy(m["tool_calls"]) {
			held = m
			break
		}
	}
	profileExec(t, db, "UPDATE messages SET tool_calls = ? WHERE session_id = ? AND id = ?",
		[]any{`[{"id": "call_0", "type": "function", "function": {"name": "terminal", "arguments": "{\"cmd\":\"winner\"}"}}]`, sid, held["_row_id"]}...)
	stale := pruneCopy(held, nil)
	delete(stale, DBRowSnapshot)
	_, err := db.RewritePrunedRows(context.Background(), sid, []PruneChange{{stale, pruneCopy(stale, "pruned stub")}}, PruneRewriteOptions{})
	var staleErr *PruneRowStaleError
	if !errors.As(err, &staleErr) {
		t.Fatalf("lens compare err = %v", err)
	}
	stamped := pruneCopy(held, nil)
	_, err = db.RewritePrunedRows(context.Background(), sid, []PruneChange{{stamped, pruneCopy(stamped, "pruned stub")}}, PruneRewriteOptions{})
	var parent *PruneRowUnresolvedError
	if !errors.As(err, &staleErr) || !errors.As(err, &parent) {
		t.Fatalf("digest compare err = %v", err)
	}
}

func paddedPruneHistory() []map[string]any {
	msgs := []map[string]any{{"role": "user", "content": "  start \n"}}
	msgs = append(msgs, pruneHistory()[1:]...)
	return append(msgs, map[string]any{"role": "assistant", "content": "  done \n"}, map[string]any{"role": "user", "content": "wrap up"})
}

func TestFreshlyResumedUnchangedTranscriptIsNotStale(t *testing.T) {
	for _, loader := range []string{"reload", "resume_projection"} {
		t.Run(loader, func(t *testing.T) {
			sid := "PRUNE_NOT_STALE_WHITESPACE"
			db := pruneDB(t, sid, paddedPruneHistory())
			var msgs []map[string]any
			if loader == "resume_projection" {
				model, _, err := db.GetResumeConversations(context.Background(), sid)
				if err != nil {
					t.Fatal(err)
				}
				msgs = model
			} else {
				msgs = archiveConversation(t, db, sid, true)
			}
			ids, err := db.RewrittenHeldRowIDs(context.Background(), sid, msgs)
			if err != nil || len(ids) != 0 {
				t.Fatalf("ids = %v err = %v", ids, err)
			}
			pruneLargeResults(t, db, sid, msgs)
		})
	}
}

func TestInPlacePruneGrowsByChangedRowsOnly(t *testing.T) {
	sid := "PRUNE_IN_PLACE_GROWTH"
	db := pruneDB(t, sid, pruneHistory())
	before := pruneRows(t, db, sid)
	msgs := archiveConversation(t, db, sid, false)
	pruned := pruneLargeResults(t, db, sid, msgs)
	var changed []int
	for i := range msgs {
		if msgs[i]["content"] != pruned[i]["content"] {
			changed = append(changed, i)
		}
	}
	after := pruneRows(t, db, sid)
	var liveIDs, beforeIDs []int64
	twins := map[any]int{}
	for _, r := range after {
		if r.active == 1 {
			liveIDs = append(liveIDs, r.id)
		} else {
			if r.compacted != 1 {
				t.Fatalf("twin not searchable: %+v", r.id)
			}
			twins[r.content]++
		}
	}
	for _, r := range before {
		beforeIDs = append(beforeIDs, r.id)
	}
	if len(after) != len(before)+len(changed) || fmt.Sprint(liveIDs) != fmt.Sprint(beforeIDs) {
		t.Fatalf("after %d rows, before %d, changed %d", len(after), len(before), len(changed))
	}
	for _, i := range changed {
		if twins[before[i].content] != 1 {
			t.Fatalf("missing twin of row %d", before[i].id)
		}
	}
	if fmt.Sprint(archiveContents(archiveConversation(t, db, sid, false))) != fmt.Sprint(archiveContents(pruned)) {
		t.Fatal("live conversation differs from the pruned list")
	}
	for _, m := range pruned {
		if m["role"] == "tool" && strings.HasPrefix(fmt.Sprint(m["content"]), "[pruned") {
			if _, ok := m[DBRowSnapshot].(string); !ok {
				t.Fatalf("replacement not stamped: %v", m["tool_call_id"])
			}
		}
	}
	if page := prunePage(t, db, sid); len(page) != len(pruned) {
		t.Fatalf("page has %d rows, want %d", len(page), len(pruned))
	}
}

func TestInPlacePruneDisplaySurvivesLaterCompactionAndBackfill(t *testing.T) {
	ctx := context.Background()
	for _, shape := range []string{"first-generation", "carried", "carried-legacy-slots"} {
		t.Run(shape, func(t *testing.T) {
			sid := "PRUNE_IN_PLACE_DISPLAY"
			db := pruneDB(t, sid, pruneHistory())
			if shape != "first-generation" {
				archiveCompact(t, db, sid, archiveConversation(t, db, sid, false), ArchiveCompactOptions{})
			}
			if shape == "carried-legacy-slots" {
				profileExec(t, db, "UPDATE messages SET display_order = NULL, display_identity = NULL WHERE session_id = ?", sid)
			}
			pruned := pruneLargeResults(t, db, sid, archiveConversation(t, db, sid, false))
			rows, err := db.readAll(ctx, "SELECT COUNT(*) AS n FROM messages WHERE session_id = ? AND (active = 1 OR compacted = 1) "+
				"AND (display_order IS NULL OR display_identity IS NULL)", sid)
			if err != nil || rows[0].Vals["n"] != int64(0) {
				t.Fatalf("unslotted rows %v err %v", rows, err)
			}
			expected := archiveContents(pruned)
			if got := prunePage(t, db, sid); fmt.Sprint(got) != fmt.Sprint(expected) {
				t.Fatalf("page after prune differs: %d vs %d rows", len(got), len(expected))
			}
			archiveCompact(t, db, sid, archiveConversation(t, db, sid, false), ArchiveCompactOptions{})
			if got := prunePage(t, db, sid); fmt.Sprint(got) != fmt.Sprint(expected) {
				t.Fatalf("page after compaction differs: %d vs %d rows", len(got), len(expected))
			}
			archiveBatch(t, db, sid, map[string]any{"role": "user", "content": "@notes.md"})
			max, err := db.readAll(ctx, "SELECT MAX(id) AS m FROM messages WHERE session_id = ?", sid)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := db.SetUserMessageContent(ctx, sid, max[0].Vals["m"].(int64), "expanded notes", nil); err != nil || n != 1 {
				t.Fatalf("set content n=%d err=%v", n, err)
			}
			if got := prunePage(t, db, sid); fmt.Sprint(got) != fmt.Sprint(append(expected, "expanded notes")) {
				t.Fatalf("page after backfill differs: %d rows", len(got))
			}
		})
	}
}

func TestInPlacePruneTwiceKeepsOneSearchHitPerUnchangedTurn(t *testing.T) {
	sid := "PRUNE_IN_PLACE_SEARCH"
	db := pruneDB(t, sid, pruneHistory())
	pruneLargeResults(t, db, sid, archiveConversation(t, db, sid, false))
	var grown []map[string]any
	for i := 8; i < 16; i++ {
		callID := fmt.Sprintf("call_%d", i)
		grown = append(grown, pruneAssistantCall(callID), map[string]any{"role": "tool", "tool_call_id": callID, "content": strings.Repeat(string(rune(65+i%26)), 24000)})
	}
	archiveBatch(t, db, sid, grown...)
	before := len(pruneRows(t, db, sid))
	second := 0
	for _, m := range archiveConversation(t, db, sid, false) {
		if s, ok := m["content"].(string); ok && len(s) > 8000 {
			second++
		}
	}
	pruneLargeResults(t, db, sid, archiveConversation(t, db, sid, false))
	if got := len(pruneRows(t, db, sid)) - before; got != second {
		t.Fatalf("grew %d rows, want %d", got, second)
	}
	hits, err := db.SearchMessages(context.Background(), "start", NewSearchMessagesOptions())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, h := range hits {
		if h.Vals["session_id"] == sid {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("hits = %d", n)
	}
}

func TestSecondPruneComparesAgainstTheFirstCommit(t *testing.T) {
	sid := "s"
	db := pruneDB(t, sid, pruneHistory())
	msgs := archiveConversation(t, db, sid, true)
	pruned := pruneLargeResults(t, db, sid, msgs)
	var target map[string]any
	for _, m := range pruned {
		if m["tool_call_id"] == "call_0" {
			target = m
		}
	}
	n, err := db.RewritePrunedRows(context.Background(), sid, []PruneChange{{pruneCopy(target, nil), pruneCopy(target, "[pruned again]")}}, PruneRewriteOptions{})
	if err != nil || n != 1 {
		t.Fatalf("second prune n=%d err=%v", n, err)
	}
	unchanged := pruneCopy(target, nil)
	if n, err := db.RewritePrunedRows(context.Background(), sid, []PruneChange{{pruneCopy(target, "[pruned again]"), unchanged}}, PruneRewriteOptions{}); err == nil || n != 0 {
		t.Fatalf("stale digest accepted n=%d err=%v", n, err)
	}
}

func TestRewritePrunedRowsGuardsAndModelConfigPatch(t *testing.T) {
	ctx := context.Background()
	sid := "s"
	db := pruneDB(t, sid, pruneHistory())
	msgs := archiveConversation(t, db, sid, true)
	tool := msgs[2]
	if _, err := db.RewritePrunedRows(ctx, sid, []PruneChange{{pruneCopy(tool, nil), map[string]any{"role": "assistant", "content": "x"}}}, PruneRewriteOptions{}); err == nil ||
		err.Error() != "a prune replacement changed the message role" {
		t.Fatalf("role change err = %v", err)
	}
	merged := pruneCopy(tool, nil)
	merged[AbsorbedRowIDs] = []any{int64(1)}
	if _, err := db.RewritePrunedRows(ctx, sid, []PruneChange{{merged, pruneCopy(tool, "x")}}, PruneRewriteOptions{}); err == nil ||
		err.Error() != "a tool message is not rewritten in place" {
		t.Fatalf("merged err = %v", err)
	}
	profileExec(t, db, "INSERT INTO session_turn_leases (conversation_id, holder, acquired_at, expires_at) VALUES (?, ?, ?, ?)", sid, "other", now(), now()+600)
	holder := "me"
	var lost *SessionTurnLeaseLostError
	if _, err := db.RewritePrunedRows(ctx, sid, []PruneChange{{pruneCopy(tool, nil), pruneCopy(tool, "x")}}, PruneRewriteOptions{TurnLeaseHolder: &holder}); !errors.As(err, &lost) {
		t.Fatalf("lease err = %v", err)
	}
	profileExec(t, db, "DELETE FROM session_turn_leases")
	patch := pyjson.NewObject()
	patch.Set("pruned", true)
	if n, err := db.RewritePrunedRows(ctx, sid, []PruneChange{{pruneCopy(tool, nil), pruneCopy(tool, "x")}}, PruneRewriteOptions{ModelConfigPatch: patch}); err != nil || n != 1 {
		t.Fatalf("patched prune n=%d err=%v", n, err)
	}
	row, err := db.GetSession(ctx, sid)
	if err != nil || !strings.Contains(fmt.Sprint(row.Vals["model_config"]), `"pruned": true`) {
		t.Fatalf("model_config = %v err %v", row.Vals["model_config"], err)
	}
	if n, err := db.RewritePrunedRows(ctx, sid, []PruneChange{{pruneCopy(msgs[4], nil), pruneCopy(msgs[4], nil)}}, PruneRewriteOptions{}); err != nil || n != 0 {
		t.Fatalf("unchanged replacement n=%d err=%v", n, err)
	}
	if _, err := db.RewritePrunedRows(ctx, sid, []PruneChange{{map[string]any{"role": "assistant", "content": "absent"}, map[string]any{"role": "assistant", "content": "y"}}}, PruneRewriteOptions{}); err == nil ||
		err.Error() != "0 live rows match a pruned assistant message" {
		t.Fatalf("unmatched err = %v", err)
	}
}

const uidCloneSQL = "INSERT INTO messages (session_id, role, content, tool_call_id, tool_calls, tool_name, " +
	"effect_disposition, timestamp, token_count, finish_reason, reasoning, reasoning_content, " +
	"reasoning_details, codex_reasoning_items, codex_message_items, platform_message_id, observed, " +
	"_compressed_summary, active, api_content, display_kind, display_metadata, message_uid, " +
	"absorbed_message_uids, tool_call_uids, tool_call_uid) " +
	"SELECT session_id, role, content, tool_call_id, tool_calls, tool_name, effect_disposition, " +
	"timestamp, token_count, finish_reason, reasoning, reasoning_content, reasoning_details, " +
	"codex_reasoning_items, codex_message_items, platform_message_id, observed, _compressed_summary, " +
	"active, api_content, display_kind, display_metadata, message_uid, absorbed_message_uids, " +
	"tool_call_uids, tool_call_uid FROM messages WHERE id = ?"

// Session of eight exchanges whose held assistant row gains a newest active clone sharing its uid
func uidFenceSession(t *testing.T) (*SessionDB, []map[string]any, any, int64, int64) {
	t.Helper()
	var history []map[string]any
	for i := range 8 {
		history = append(history, map[string]any{"role": "user", "content": fmt.Sprintf("question %d", i)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d", i)})
	}
	db := pruneDB(t, "s", history)
	held, err := db.GetMessagesAsConversation(context.Background(), "s", ConversationOptions{RepairAlternation: true})
	if err != nil {
		t.Fatal(err)
	}
	var uid any
	for _, m := range held {
		if _, has := m["_row_id"]; has {
			t.Fatal("held dict carries a row id")
		}
		if uid == nil && m["role"] == "assistant" && pyTruthy(m["content"]) {
			uid = m[MessageUID]
		}
	}
	ids := func() []int64 {
		rows, err := db.readAll(context.Background(), "SELECT id FROM messages WHERE session_id = ? AND active = 1 AND message_uid = ? ORDER BY id", "s", uid)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range rows {
			out = append(out, r.Vals["id"].(int64))
		}
		return out
	}
	profileExec(t, db, uidCloneSQL, ids()[len(ids())-1])
	both := ids()
	if len(both) != 2 || both[1] <= both[0] {
		t.Fatalf("uid rows = %v", both)
	}
	return db, held, uid, both[0], both[1]
}

func TestUIDFenceCASAgainstTheNewestActiveCopy(t *testing.T) {
	db, held, uid, _, cloneID := uidFenceSession(t)
	resume, err := db.GetMessagesAsConversation(context.Background(), "s", ConversationOptions{IncludeRowIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	for _, m := range resume {
		if id, _ := pyIntID(m["_row_id"]); m[MessageUID] == uid && id == cloneID {
			original = m
		}
	}
	if original == nil {
		t.Fatal("clone not materialized")
	}
	if _, err := db.RewritePrunedRows(context.Background(), "s", []PruneChange{{pruneCopy(original, nil), pruneCopy(original, "[pruned by the other surface]")}}, PruneRewriteOptions{}); err != nil {
		t.Fatal(err)
	}
	ids, err := db.RewrittenHeldRowIDs(context.Background(), "s", held)
	if err != nil || fmt.Sprint(ids) != fmt.Sprint([]int64{cloneID}) {
		t.Fatalf("ids = %v err = %v", ids, err)
	}
}

func TestUIDFenceKeepsHeldDigestCurrentWhenOnlyAnOlderDuplicateMoved(t *testing.T) {
	db, held, _, originalID, _ := uidFenceSession(t)
	profileExec(t, db, "UPDATE messages SET content = ? WHERE id = ?", "[edited in place by another writer]", originalID)
	ids, err := db.RewrittenHeldRowIDs(context.Background(), "s", held)
	if err != nil || len(ids) != 0 {
		t.Fatalf("ids = %v err = %v", ids, err)
	}
}
