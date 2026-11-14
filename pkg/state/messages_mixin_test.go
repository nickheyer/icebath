package state

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func reactionSession(t *testing.T) (*SessionDB, string, []int64) {
	t.Helper()
	d := usageDB(t)
	ctx := context.Background()
	if err := d.CreateSession(NewSession{ID: "react-test", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	var rows []int64
	for _, m := range [][2]string{{"user", "how do i center a div"}, {"assistant", "use flexbox"}} {
		id, err := d.AppendMessage(ctx, "react-test", map[string]any{"role": m[0], "content": m[1]}, AppendGuards{})
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, id)
	}
	return d, "react-test", rows
}

func reactionField(rs []any, key string) []string {
	var out []string
	for _, r := range rs {
		v, _ := mappingGet(r, key)
		out = append(out, pyStr(v))
	}
	return out
}

func react(t *testing.T, d *SessionDB, key string, row int64, emoji *string, author string) ([]any, bool) {
	t.Helper()
	rs, ok, err := d.SetMessageReaction(context.Background(), key, row, emoji, author)
	if err != nil {
		t.Fatal(err)
	}
	return rs, ok
}

func TestReactionTapbackSemantics(t *testing.T) {
	d, key, rows := reactionSession(t)
	react(t, d, key, rows[0], strPtr("❤️"), "user")
	if rs, _ := react(t, d, key, rows[0], strPtr("\U0001f602"), "user"); strings.Join(reactionField(rs, "emoji"), ",") != "\U0001f602" {
		t.Fatalf("one per author %v", rs)
	}
	react(t, d, key, rows[0], strPtr("\U0001f44d"), "user")
	if rs, ok := react(t, d, key, rows[0], strPtr("\U0001f44d"), "user"); !ok || len(rs) != 0 {
		t.Fatalf("repeat retracts %v %v", rs, ok)
	}
	if got, err := d.GetMessageReactions(context.Background(), key, rows[0]); err != nil || len(got) != 0 {
		t.Fatalf("stored %v %v", got, err)
	}
	react(t, d, key, rows[0], strPtr("❤️"), "user")
	if rs, _ := react(t, d, key, rows[0], strPtr("\U0001f525"), "agent"); strings.Join(reactionField(rs, "author"), ",") != "user,agent" {
		t.Fatalf("independent authors %v", rs)
	}
	if rs, _ := react(t, d, key, rows[0], nil, "user"); strings.Join(reactionField(rs, "author"), ",") != "agent" {
		t.Fatalf("clear one author %v", rs)
	}
	react(t, d, key, rows[0], nil, "agent")
	row, err := d.readOne(context.Background(), "SELECT display_metadata FROM messages WHERE id = ?", rows[0])
	if err != nil || rowValue(row, "display_metadata") != nil {
		t.Fatalf("cleared metadata %v %v", row, err)
	}
	if _, ok := react(t, d, key, 9999, strPtr("x"), "user"); ok {
		t.Fatal("foreign row accepted")
	}
	if _, ok := react(t, d, "no-such-session", rows[0], strPtr("x"), "user"); ok {
		t.Fatal("foreign session accepted")
	}
}

func TestUnseenReactionsTakenOnce(t *testing.T) {
	d, key, rows := reactionSession(t)
	ctx := context.Background()
	react(t, d, key, rows[1], strPtr("❤️"), "user")
	react(t, d, key, rows[0], strPtr("\U0001f60a"), "agent")
	first, err := d.TakeUnseenReactions(ctx, key, "user")
	if err != nil || len(first) != 1 || first[0]["emoji"] != "❤️" || first[0]["row_id"] != rows[1] || first[0]["text"] != "use flexbox" {
		t.Fatalf("first take %v %v", first, err)
	}
	if again, _ := d.TakeUnseenReactions(ctx, key, "user"); len(again) != 0 {
		t.Fatalf("taken twice %v", again)
	}
	react(t, d, key, rows[1], strPtr("\U0001f525"), "user")
	if rearmed, _ := d.TakeUnseenReactions(ctx, key, "user"); len(rearmed) != 1 || rearmed[0]["emoji"] != "\U0001f525" {
		t.Fatalf("rearm %v", rearmed)
	}
	content, _ := d.readOne(ctx, "SELECT content FROM messages WHERE id = ?", rows[1])
	if rowValue(content, "content") != "use flexbox" {
		t.Fatalf("content mutated %v", content)
	}
}

func TestLatestMessageRowIDTargetsRole(t *testing.T) {
	d, key, rows := reactionSession(t)
	ctx := context.Background()
	if id, ok, _ := d.LatestMessageRowID(ctx, key, "user", 0, true); !ok || id != rows[0] {
		t.Fatalf("latest user %d %v", id, ok)
	}
	newest, _ := d.AppendMessage(ctx, key, map[string]any{"role": "user", "content": "thanks!"}, AppendGuards{})
	_, _ = d.AppendMessage(ctx, key, map[string]any{"role": "assistant", "content": "you're welcome"}, AppendGuards{})
	if id, _, _ := d.LatestMessageRowID(ctx, key, "user", 0, true); id != newest {
		t.Fatalf("newest user %d", id)
	}
	if id, _, _ := d.LatestMessageRowID(ctx, key, "assistant", 0, true); id == newest {
		t.Fatal("assistant resolved to user row")
	}
	if role, _ := d.LatestConversationRole(ctx, key); role != "assistant" {
		t.Fatalf("latest role %q", role)
	}
	if role, ok, _ := d.GetMessageRole(ctx, key, newest); !ok || role != "user" {
		t.Fatalf("role %q", role)
	}
	if n, _ := d.MessageCount(ctx, key); n != 4 {
		t.Fatalf("count %d", n)
	}
	if err := d.ClearMessages(ctx, key); err != nil {
		t.Fatal(err)
	}
	row, _ := d.GetSession(ctx, key)
	if n, _ := d.MessageCount(ctx, key); n != 0 || pyStr(rowValue(row, "message_count")) != "0" {
		t.Fatalf("cleared %d %v", n, row)
	}
}

func TestAppendMessageCountersAndClosedParent(t *testing.T) {
	d := usageDB(t)
	ctx := context.Background()
	_ = d.CreateSession(NewSession{ID: "p", Source: "cli"})
	calls := []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "todo", "arguments": "{}"}}}
	if _, err := d.AppendMessage(ctx, "p", map[string]any{"role": "assistant", "content": "", "tool_calls": calls, "message_id": "ignored"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	row, _ := d.readOne(ctx, "SELECT tool_call_uids, platform_message_id, message_uid FROM messages WHERE session_id = 'p'")
	if !strings.Contains(pyStr(rowValue(row, "tool_call_uids")), `"c1"`) || rowValue(row, "platform_message_id") != nil || !pyTruthy(rowValue(row, "message_uid")) {
		t.Fatalf("identity columns %v", row)
	}
	sess, _ := d.GetSession(ctx, "p")
	if pyStr(rowValue(sess, "message_count")) != "1" || pyStr(rowValue(sess, "tool_call_count")) != "1" {
		t.Fatalf("counters %v", sess)
	}
	if _, err := d.writeRowcount(ctx, "UPDATE sessions SET ended_at = 1, end_reason = 'compression' WHERE id = 'p'", nil, nil); err != nil {
		t.Fatal(err)
	}
	_, err := d.AppendMessage(ctx, "p", map[string]any{"role": "user", "content": "late"}, AppendGuards{})
	if _, ok := err.(*CompressionSessionClosedError); !ok {
		t.Fatalf("closed parent append %v", err)
	}
	holder := "me"
	_ = d.CreateSession(NewSession{ID: "q", Source: "cli"})
	_, err = d.AppendMessage(ctx, "q", map[string]any{"role": "user", "content": "x"}, AppendGuards{TurnLeaseHolder: &holder})
	if _, ok := err.(*SessionTurnLeaseLostError); !ok {
		t.Fatalf("lease lost %v", err)
	}
}

func TestUpdateSessionToolNamesStoresContentAddressedPin(t *testing.T) {
	d := usageDB(t)
	ctx := context.Background()
	_ = d.CreateSession(NewSession{ID: "a", Source: "cli"})
	_ = d.CreateSession(NewSession{ID: "b", Source: "cli"})
	pin := []any{map[string]any{"name": "terminal"}}
	for _, id := range []string{"a", "b"} {
		if err := d.UpdateSessionToolNames(id, pin); err != nil {
			t.Fatal(err)
		}
	}
	row, _ := d.GetSession(ctx, "a")
	if rowValue(row, "tool_names") != `[{"name": "terminal"}]` {
		t.Fatalf("resolved pin %v", row)
	}
	count, _ := d.readOne(ctx, "SELECT COUNT(*) AS n FROM system_prompts")
	if n, _ := rowInt(count, "n"); n != 1 {
		t.Fatalf("pin rows %d", n)
	}
	raw, _ := d.readOne(ctx, "SELECT tool_names FROM sessions WHERE id = 'a'")
	if len(pyStr(rowValue(raw, "tool_names"))) != 64 {
		t.Fatalf("column holds hash %v", raw)
	}
	_ = d.UpdateSessionToolNames("a", nil)
	_ = d.UpdateSessionToolNames("b", nil)
	count, _ = d.readOne(ctx, "SELECT COUNT(*) AS n FROM system_prompts")
	if n, _ := rowInt(count, "n"); n != 0 {
		t.Fatalf("unreferenced pin kept %d", n)
	}
	prompt := "be brief"
	if err := d.UpdateSystemPrompt(ctx, "a", &prompt); err != nil {
		t.Fatal(err)
	}
	row, _ = d.GetSession(ctx, "a")
	if rowValue(row, "system_prompt") != "be brief" {
		t.Fatalf("prompt %v", row)
	}
}

func TestDetachedResultsRecordOnceAcrossLineage(t *testing.T) {
	d := usageDB(t)
	ctx := context.Background()
	_ = d.CreateSession(NewSession{ID: "parent", Source: "cli"})
	meta := pyjson.NewObject()
	meta.Set("delegation_id", "del-1")
	meta.Set("delivery_notice", "done")
	first, err := d.AppendDelegationDelivery(ctx, "parent", "result text", meta)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.writeRowcount(ctx, "UPDATE sessions SET end_reason = 'compression' WHERE id = 'parent'", nil, nil)
	_ = d.CreateSession(NewSession{ID: "child", Source: "cli", ParentSessionID: "parent"})
	again, err := d.AppendDelegationDelivery(ctx, "child", "result text", meta)
	if err != nil || again != first {
		t.Fatalf("replay after rotation %d %d %v", first, again, err)
	}
	if _, err := d.AppendDelegationDelivery(ctx, "child", "x", pyjson.NewObject()); err == nil || err.Error() != "Delegation delivery requires a stable delegation_id" {
		t.Fatalf("missing id %v", err)
	}
	note, err := d.AppendCompletionNote(ctx, "child", "adm-1", "note", "local_completion", pyjson.NewObject())
	if err != nil {
		t.Fatal(err)
	}
	if repeat, _ := d.AppendCompletionNote(ctx, "child", "adm-1", "note", "local_completion", pyjson.NewObject()); repeat != note {
		t.Fatalf("note twice %d %d", note, repeat)
	}
	row, _ := d.readOne(ctx, "SELECT display_kind, display_metadata FROM messages WHERE id = ?", note)
	if rowValue(row, "display_metadata") != `{"completion_admission": "adm-1"}` {
		t.Fatalf("note metadata %v", row)
	}
}

func TestLatestTodoResultIsPairedAndValid(t *testing.T) {
	d := usageDB(t)
	ctx := context.Background()
	_ = d.CreateSession(NewSession{ID: "t", Source: "cli"})
	add := func(m map[string]any) int64 {
		id, err := d.AppendMessage(ctx, "t", m, AppendGuards{})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	call := func(id string) []any {
		return []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "todo", "arguments": "{}"}}}
	}
	add(map[string]any{"role": "user", "content": "plan", "timestamp": 1000.0})
	add(map[string]any{"role": "assistant", "content": "", "tool_calls": call("c1"), "timestamp": 1001.0})
	valid := `{"todos": [{"id": "1", "content": "a", "status": "pending"}], "revision": 2}`
	add(map[string]any{"role": "tool", "tool_name": "todo", "tool_call_id": "c1", "content": valid, "timestamp": 1002.0})
	add(map[string]any{"role": "assistant", "content": "", "tool_calls": call("c2"), "timestamp": 1003.0})
	add(map[string]any{"role": "tool", "tool_name": "todo", "tool_call_id": "c2", "content": `{"todos": "bad"}`, "timestamp": 1004.0})
	got, ok, err := d.GetLatestTodoResult(ctx, "t", nil)
	if err != nil || !ok || got != valid {
		t.Fatalf("latest todo %q %v %v", got, ok, err)
	}
	small := 10
	if _, ok, _ := d.GetLatestTodoResult(ctx, "t", &small); ok {
		t.Fatal("oversized result restored")
	}
}

func TestSplitUserOriginatedTurnSeparatesHandoff(t *testing.T) {
	carrier := map[string]any{"role": "user", "content": MergedPriorContextHeader + "\nhello there\n" + MergedSummaryDelimiter + "\n" + SummaryPrefix + " body\n" + SummaryEndMarker,
		"display_metadata": map[string]any{"reactions": []any{"r"}, "other": 1}}
	handoff, live := SplitUserOriginatedTurn(carrier)
	if handoff == nil || live == nil || live["content"] != "hello there" || handoff["display_kind"] != "hidden" {
		t.Fatalf("split %v %v", handoff, live)
	}
	if meta, _ := live["display_metadata"].(map[string]any); len(meta) != 1 || meta["reactions"] == nil {
		t.Fatalf("durable metadata %v", live["display_metadata"])
	}
	if h, l := SplitUserOriginatedTurn(map[string]any{"role": "user", "content": EmptyToolResponseNudge}); h != nil || l != nil {
		t.Fatalf("nudge is no human turn %v %v", h, l)
	}
	if _, l := SplitUserOriginatedTurn(map[string]any{"role": "user", "content": "hi", "display_kind": SteerDisplayKind}); l == nil || l["display_kind"] != nil {
		t.Fatalf("steer keeps authority %v", l)
	}
}

func TestLaterToolCopyIDsSkipsStubAfterArchivedOriginal(t *testing.T) {
	mk := func(id, active, compacted, order int64) *pyjson.Object {
		o := pyjson.NewObject()
		for _, kv := range [][2]any{{"id", id}, {"role", "tool"}, {"session_id", "s"}, {"message_uid", "u"}, {"timestamp", 1.0},
			{"active", active}, {"compacted", compacted}, {"display_order", order}, {"display_metadata", nil}} {
			o.Set(kv[0].(string), kv[1])
		}
		return o
	}
	got := laterToolCopyIDs([]*pyjson.Object{mk(1, 0, 1, 5), mk(2, 1, 0, 9), mk(3, 1, 0, 5)})
	if !got[2] || got[3] || got[1] || len(got) != 1 {
		t.Fatalf("later copies %v", got)
	}
	if !strings.Contains(notALaterToolCopySQL("m"), "+original.display_order < m.display_order") {
		t.Fatal("sql shape")
	}
}

func TestMessageProbesAndBoundaries(t *testing.T) {
	d := usageDB(t)
	ctx := context.Background()
	_ = d.CreateSession(NewSession{ID: "g", Source: "telegram", SessionKey: "peer"})
	meta := pyjson.NewObject()
	meta.Set("gateway_input_owner", "own-1")
	first, _ := d.AppendMessage(ctx, "g", map[string]any{"role": "user", "content": "hi", "platform_message_id": "pm-1", "display_metadata": meta}, AppendGuards{})
	if ok, _ := d.HasGatewayInputOwner(ctx, "g", "own-1"); !ok {
		t.Fatal("owner marker")
	}
	if ok, _ := d.HasPlatformMessageID(ctx, "g", "pm-1"); !ok {
		t.Fatal("platform id")
	}
	if ok, _ := d.HasPlatformMessageID(ctx, "g", "pm-2"); ok {
		t.Fatal("unknown platform id")
	}
	if matched, err := d.SetLatestMatchingMessageDisplayKind(ctx, "g", "user", "hi", "steer", nil); err != nil || !matched {
		t.Fatalf("display kind stamp %v %v", matched, err)
	}
	if ids, _ := d.GetActiveMessageIDs(ctx, "g"); len(ids) != 1 || ids[0] != first {
		t.Fatalf("active ids %v", ids)
	}
	if _, ok, _ := d.LatestConversationBoundary(ctx, "peer", "telegram"); ok {
		t.Fatal("boundary before reset")
	}
	for _, reason := range []string{"compression", "session_reset", "idle"} {
		if err := d.executeWrite(ctx, func(conn SQLiteConn) error { return bumpConversationGeneration(ctx, conn, "g", reason) }, nil); err != nil {
			t.Fatal(err)
		}
	}
	if n, ok, _ := d.LatestConversationBoundary(ctx, "peer", "telegram"); !ok || n != 2 {
		t.Fatalf("generation %d %v", n, ok)
	}
}

func TestDropShadowedCheckpointRowsKeepsNewest(t *testing.T) {
	d := usageDB(t)
	ctx := context.Background()
	_ = d.CreateSession(NewSession{ID: "c", Source: "cli"})
	items := func(tag string) []any {
		return []any{map[string]any{"type": "compaction", "id": tag}, map[string]any{"type": "reasoning", "id": tag}}
	}
	old, _ := d.AppendMessage(ctx, "c", map[string]any{"role": "assistant", "content": "a", "codex_reasoning_items": items("old")}, AppendGuards{})
	carrier, _ := d.AppendMessage(ctx, "c", map[string]any{"role": "assistant", "content": "b", "codex_reasoning_items": items("new")}, AppendGuards{})
	var rewritten int
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		rewritten, err = 0, pruneShadowedCheckpoints(ctx, conn, "c", []map[string]any{{"role": "assistant", "codex_reasoning_items": items("new"), "_row_id": carrier}})
		return err
	}, nil)
	row, _ := d.readOne(ctx, "SELECT codex_reasoning_items FROM messages WHERE id = ?", old)
	if err != nil || rewritten != 0 || rowValue(row, "codex_reasoning_items") != `[{"id": "old", "type": "reasoning"}]` {
		t.Fatalf("pruned %v %v", row, err)
	}
	if n, _, _ := activeCounts(t, d, "c"); n != 2 {
		t.Fatalf("active %d", n)
	}
}

func activeCounts(t *testing.T, d *SessionDB, sid string) (int, int, error) {
	t.Helper()
	var m, c int
	err := d.readCtx(context.Background(), true, func(conn SQLiteConn) error {
		var err error
		m, c, err = activeTranscriptCounts(context.Background(), conn, sid)
		return err
	})
	return m, c, err
}

func TestDedupeDisplayGenerationsPrefersLiveCopyAndHidesRemoved(t *testing.T) {
	mk := func(id int64, active, compacted int64, content string, ts float64) *pyjson.Object {
		o := pyjson.NewObject()
		for _, kv := range [][2]any{{"id", id}, {"role", "user"}, {"content", content}, {"timestamp", ts}, {"tool_call_id", nil},
			{"tool_calls", nil}, {"tool_name", nil}, {"display_kind", nil}, {"display_metadata", nil}, {"message_uid", nil},
			{"absorbed_message_uids", nil}, {"_compressed_summary", 0}, {"active", active}, {"compacted", compacted}, {"display_identity", nil}} {
			o.Set(kv[0].(string), kv[1])
		}
		return o
	}
	rows := []*pyjson.Object{mk(1, 0, 1, "hello", 1), mk(2, 0, 1, "next", 2), mk(3, 1, 0, "hello", 1)}
	got, err := dedupeDisplayGenerations(rows, false)
	if err != nil || len(got) != 2 || rowValue(got[0], "id") != int64(3) || rowValue(got[1], "id") != int64(2) {
		t.Fatalf("dedupe %v %v", dumpRows(got), err)
	}
	gone := mk(4, 0, 0, "next", 2)
	got, err = dedupeDisplayGenerations(append(rows, gone), true)
	if err != nil || len(got) != 1 || rowValue(got[0], "id") != int64(3) {
		t.Fatalf("removed copy shown %v %v", dumpRows(got), err)
	}
}
