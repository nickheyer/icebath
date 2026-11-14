package state

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func readRawTimestamps(t *testing.T, db *SessionDB, sid string) []any {
	t.Helper()
	rows, err := db.readAll(context.Background(), "SELECT timestamp FROM messages WHERE session_id = ? ORDER BY id", sid)
	if err != nil {
		t.Fatal(err)
	}
	var out []any
	for _, r := range rows {
		out = append(out, rowValue(r, "timestamp"))
	}
	return out
}

func readSession(t *testing.T, db *SessionDB, n NewSession) {
	t.Helper()
	if err := db.CreateSession(n); err != nil {
		t.Fatal(err)
	}
}

func readAppend(t *testing.T, db *SessionDB, sid string, fields map[string]any) int64 {
	t.Helper()
	id, err := db.AppendMessage(context.Background(), sid, fields, AppendGuards{})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReplaceMessagesPreservesTimestamps(t *testing.T) {
	db := usageDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	in := []map[string]any{
		{"role": "user", "content": "first", "timestamp": 100.0},
		{"role": "assistant", "content": "second", "timestamp": 200.0},
		{"role": "user", "content": "third", "timestamp": 300.0},
	}
	if err := db.ReplaceMessages(ctx, "s1", in, ReplaceOptions{}); err != nil {
		t.Fatal(err)
	}
	out, err := db.GetMessages(ctx, "s1", GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got []any
	for _, m := range out {
		v, _ := m.Get("timestamp")
		got = append(got, v)
	}
	want := []any{100.0, 200.0, 300.0}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(readRawTimestamps(t, db, "s1"), want) {
		t.Fatalf("%v %v", got, readRawTimestamps(t, db, "s1"))
	}
}

func TestCompressionReplaceRoundtripPreservesTimestamps(t *testing.T) {
	db := usageDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	stamps := []float64{1_500_000_000.0, 1_500_000_100.0, 1_500_000_200.0}
	for i, ts := range stamps {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		readAppend(t, db, "s1", map[string]any{"role": role, "content": "msg-" + string(rune('0'+i)), "timestamp": ts})
	}
	history, err := db.GetMessagesAsConversation(ctx, "s1", ConversationOptions{})
	if err != nil || len(history) != 3 {
		t.Fatal(history, err)
	}
	compressed := append([]map[string]any{{"role": "user", "content": "[summary]"}}, history[1:]...)
	if err := db.ReplaceMessages(ctx, "s1", compressed, ReplaceOptions{}); err != nil {
		t.Fatal(err)
	}
	raw := readRawTimestamps(t, db, "s1")
	if len(raw) != 3 || raw[1] != stamps[1] || raw[2] != stamps[2] || raw[0].(float64) <= stamps[2] {
		t.Fatalf("%v", raw)
	}
}

func TestReplaceMessagesPreservesDisplayMetadata(t *testing.T) {
	db := usageDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	meta := map[string]any{"task_count": int64(3), "delegation_id": "del-2", "duration_seconds": 12.5}
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "event", "display_kind": "async_delegation_complete", "display_metadata": meta})
	conv, err := db.GetMessagesAsConversation(ctx, "s1", ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceMessages(ctx, "s1", conv, ReplaceOptions{}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := db.GetMessagesAsConversation(ctx, "s1", ConversationOptions{})
	if err != nil || len(reloaded) != 1 {
		t.Fatal(reloaded, err)
	}
	if reloaded[0]["display_kind"] != "async_delegation_complete" || !pyValueEqual(reloaded[0]["display_metadata"], meta) {
		t.Fatalf("%v", reloaded[0])
	}
}

func TestGetMessagesPagingAndDisplayReads(t *testing.T) {
	db := usageDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	var ids []int64
	for i := range 6 {
		ids = append(ids, readAppend(t, db, "s1", rewriteTurn(i, "m")))
	}
	contents := func(rows []map[string]any) []string {
		var out []string
		for _, m := range rows {
			out = append(out, pyStr(m["content"]))
		}
		return out
	}
	page := func(o GetMessagesOptions) []string {
		t.Helper()
		rows, err := db.GetMessages(ctx, "s1", o)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			v, _ := r.Get("content")
			out = append(out, pyStr(v))
		}
		return out
	}
	two := 2
	if got := page(GetMessagesOptions{Limit: &two, Latest: true}); !slices.Equal(got, []string{"m 4", "m 5"}) {
		t.Fatal(got)
	}
	if got := page(GetMessagesOptions{Limit: &two, Offset: 1}); !slices.Equal(got, []string{"m 1", "m 2"}) {
		t.Fatal(got)
	}
	if got := page(GetMessagesOptions{AfterID: &ids[3]}); !slices.Equal(got, []string{"m 4", "m 5"}) {
		t.Fatal(got)
	}
	for _, bad := range []GetMessagesOptions{{AfterID: &ids[0], Latest: true}, {AfterID: &ids[0], IncludeCompacted: true}, {AfterID: &ids[0], IncludeAncestors: true}} {
		var ve *stateValueError
		if _, err := db.GetMessages(ctx, "s1", bad); !errors.As(err, &ve) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if _, err := db.DeactivateMessage(ctx, "s1", ids[5]); err != nil {
		t.Fatal(err)
	}
	if got := page(GetMessagesOptions{}); len(got) != 5 {
		t.Fatal(got)
	}
	if got := page(GetMessagesOptions{IncludeInactive: true}); len(got) != 6 {
		t.Fatal(got)
	}
	if got := page(GetMessagesOptions{IncludeCompacted: true, Limit: &two, Latest: true}); !slices.Equal(got, []string{"m 3", "m 4"}) {
		t.Fatal(got)
	}
	missing, err := db.readOne(ctx, displayIndexMissingSQL, "s1")
	if err != nil || missing != nil {
		t.Fatalf("display index left incomplete %v %v", missing, err)
	}
	legacy, err := db.legacyDisplayPage(ctx, "s1", activeClause(false, true), nil, 0, false)
	if err != nil || len(legacy) != 5 {
		t.Fatal(legacy, err)
	}
	var shown []map[string]any
	if err := db.readCtx(ctx, false, func(conn SQLiteConn) error {
		rows, ok, err := db.displayMessagesFromConn(ctx, conn, "s1")
		if !ok {
			t.Fatal("display read refused")
		}
		for _, r := range rows {
			shown = append(shown, objMap(r))
		}
		return err
	}); err != nil || len(shown) != 5 {
		t.Fatal(shown, err)
	}
	if got := contents(shown); got[0] != "m 0" || got[4] != "m 4" {
		t.Fatal(got)
	}
	rev, err := db.ConversationRevision(ctx, "s1", false)
	if err != nil || rev.Count != 5 || rev.MaxID != ids[4] || !slices.Equal(rev.SessionIDs, []string{"s1"}) {
		t.Fatalf("%+v %v", rev, err)
	}
	if n, err := db.GetResumeMessageCount(ctx, "s1", true); err != nil || n != 5 {
		t.Fatal(n, err)
	}
	limit := 3
	var tooLarge *SessionResumeTooLargeError
	if _, err := db.AssertResumeSafe(ctx, "s1", &limit, true); !errors.As(err, &tooLarge) || tooLarge.MessageCount != 4 || tooLarge.Scope != "in its tip segment" {
		t.Fatalf("%v", err)
	}
	limit = 10
	if n, err := db.AssertResumeSafe(ctx, "s1", &limit, false); err != nil || n != 5 {
		t.Fatal(n, err)
	}
	limit = -1
	if _, err := db.AssertResumeSafe(ctx, "s1", &limit, false); err == nil {
		t.Fatal("negative cap accepted")
	}
}

func TestConversationRowsCarryIdentityAndToolUIDs(t *testing.T) {
	db := usageDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "go"})
	readAppend(t, db, "s1", map[string]any{"role": "assistant", "content": "", "tool_calls": []any{seqCall("c1")}, "reasoning_content": "think"})
	readAppend(t, db, "s1", map[string]any{"role": "tool", "content": "out", "tool_call_id": "c1", "tool_name": "f"})
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "next"})
	conv, err := db.GetMessagesAsConversation(ctx, "s1", ConversationOptions{IncludeRowIDs: true})
	if err != nil || len(conv) != 4 {
		t.Fatal(conv, err)
	}
	for _, m := range conv {
		if m[DBPersistedMarker] != true || m["_row_id"] == nil {
			t.Fatalf("%v", m)
		}
	}
	if conv[1]["reasoning_content"] != "think" || conv[2]["tool_call_id"] != "c1" || conv[2]["tool_name"] != "f" {
		t.Fatalf("%v", conv)
	}
	if uid, _ := conv[2][ToolCallUID].(string); uid == "" {
		t.Fatalf("tool result not paired to its call uid %v", conv[2])
	}
	repaired, err := db.GetMessagesAsConversation(ctx, "s1", ConversationOptions{RepairAlternation: true})
	if err != nil || repaired[0][DBRowSnapshot] == nil || repaired[0]["_row_id"] != nil {
		t.Fatal(repaired, err)
	}
	model, display, err := db.GetResumeConversations(ctx, "s1")
	if err != nil || len(model) != 4 || len(display) != 4 {
		t.Fatal(model, display, err)
	}
	if prefix, err := db.GetAncestorDisplayPrefix(ctx, "s1"); err != nil || len(prefix) != 0 {
		t.Fatal(prefix, err)
	}
	if root, err := db.GetConversationRoot("s1"); err != nil || root != "s1" {
		t.Fatal(root, err)
	}
}

func TestCompressionTipAndReplayedUserDedupe(t *testing.T) {
	db := usageDB(t)
	ctx := context.Background()
	readSession(t, db, NewSession{ID: "root", Source: "cli"})
	readAppend(t, db, "root", map[string]any{"role": "user", "content": "hello", "timestamp": 10.0})
	readAppend(t, db, "root", map[string]any{"role": "assistant", "content": "hi"})
	readAppend(t, db, "root", map[string]any{"role": "user", "content": "carry", "timestamp": 20.0})
	if err := db.writeSQL(ctx, "UPDATE sessions SET end_reason = 'compression', ended_at = 5 WHERE id = 'root'", nil, false, nil); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "tip", Source: "cli", ParentSessionID: "root"})
	readAppend(t, db, "tip", map[string]any{"role": "user", "content": "carry", "timestamp": 20.0})
	readAppend(t, db, "tip", map[string]any{"role": "assistant", "content": "done"})
	if tip, err := db.GetCompressionTip(ctx, "root"); err != nil || tip != "tip" {
		t.Fatal(tip, err)
	}
	if chain, err := db.GetCompressionChain(ctx, "root"); err != nil || !slices.Equal(chain, []string{"root", "tip"}) {
		t.Fatal(chain, err)
	}
	if err := db.readCtx(ctx, false, func(conn SQLiteConn) error {
		tip, err := compressionTipOn(ctx, conn, "root")
		if tip != "tip" {
			t.Fatal(tip)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := db.ResolveResumeSessionID("root"); err != nil || got != "tip" {
		t.Fatal(got, err)
	}
	if root, err := db.GetConversationRoot("tip"); err != nil || root != "root" {
		t.Fatal(root, err)
	}
	merged, err := db.GetMessagesAsConversation(ctx, "tip", ConversationOptions{IncludeAncestors: true})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range merged {
		texts = append(texts, pyStr(m["content"]))
	}
	if !slices.Equal(texts, []string{"hello", "hi", "carry", "done"}) {
		t.Fatalf("replayed user not deduped %v", texts)
	}
	prefix, err := db.GetAncestorDisplayPrefix(ctx, "tip")
	if err != nil || len(prefix) == 0 || prefix[0]["_row_id"] != nil || prefix[0]["content"] != "hello" {
		t.Fatal(prefix, err)
	}
}

func TestReplayedUserCloneHelpers(t *testing.T) {
	if _, ok := exactReplayedUserCloneKey(nil, "x"); ok {
		t.Fatal("keyed without timestamp")
	}
	a, _ := exactReplayedUserCloneKey(int64(5), map[string]any{"b": 1, "a": "é"})
	b, _ := exactReplayedUserCloneKey(5.0, map[string]any{"a": "é", "b": 1})
	if a != b || a == "" {
		t.Fatalf("%q %q", a, b)
	}
	msgs := []map[string]any{seqUser("same"), seqAssistant("")}
	if i, prefer, ok := findDuplicateReplayedUserMessage(msgs, seqUser("same")); !ok || i != 0 || prefer {
		t.Fatal(i, prefer, ok)
	}
	msgs = []map[string]any{seqUser("same"), seqAssistant("answered")}
	if _, _, ok := findDuplicateReplayedUserMessage(msgs, seqUser("same")); ok {
		t.Fatal("matched across an answered assistant turn")
	}
	if got := pyLatestPage([]int{1, 2, 3, 4, 5}, 1, nil, true); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Fatal(got)
	}
	neg := -1
	if got := pyPage([]int{1, 2, 3}, 0, &neg); !slices.Equal(got, []int{1, 2}) {
		t.Fatal(got)
	}
}
