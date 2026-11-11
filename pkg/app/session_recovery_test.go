package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Points the CJK tokenizer at an absent extension, the state of a hermes test home without libfts5_cjk.so
func recoveryWithoutCJKExtension(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_FTS5_CJK_SO", filepath.Join(t.TempDir(), "lib", "libfts5_cjk.so"))
}

// Raw autocommit connection to a test database, closed when the test ends
func recoveryTestConn(t *testing.T, path string) state.SQLiteConn {
	t.Helper()
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Runs statements on a raw connection
func recoveryTestExec(t *testing.T, conn state.SQLiteConn, query string, args ...any) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// Three titled sessions of seven messages each with topic, routing, delegation and derived FTS markers
func recoveryMakeSource(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		sid := fmt.Sprintf("recovery-session-%d", n)
		if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Cwd: fmt.Sprintf("/tmp/recovery-%d", n)}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SetSessionTitle(ctx, sid, fmt.Sprintf("Recovery %d", n)); err != nil {
			t.Fatal(err)
		}
		for m := 0; m < 7; m++ {
			role := "assistant"
			if m%2 == 0 {
				role = "user"
			}
			if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": role, "content": fmt.Sprintf("recoverable payload %d %d", n, m)}, state.AppendGuards{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.SetMeta(ctx, "goal:recovery-session-0", `{"status":"active"}`); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyTelegramTopicMigration(ctx); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"fts_rebuild_high_water": "999", "fts_rebuild_progress": "500", "fts_tool_full_content_high_water": "7"} {
		if err := db.SetMeta(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	recoveryTestExec(t, conn, `INSERT INTO telegram_dm_topic_mode (chat_id, user_id, enabled, activated_at, updated_at) VALUES (?, ?, 1, ?, ?)`,
		"chat-1", "user-1", 1.0, 2.0)
	recoveryTestExec(t, conn, `INSERT INTO telegram_dm_topic_bindings (chat_id, thread_id, user_id, session_key, session_id,
		managed_mode, linked_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"chat-1", "thread-1", "user-1", "telegram:user-1:chat-1", "recovery-session-0", "auto", 1.0, 2.0)
	recoveryTestExec(t, conn, `INSERT INTO gateway_routing (scope, session_key, entry_json, updated_at) VALUES (?, ?, ?, ?)`,
		"telegram", "telegram:user-1:chat-1", "{}", 2.0)
	recoveryTestExec(t, conn, `INSERT INTO async_delegations (delegation_id, origin_session, state, dispatched_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"delegation-1", "recovery-session-0", "completed", 1.0, 2.0)
}

// Value at a dotted path of nested report objects
func recoveryAt(t *testing.T, report *pyjson.Object, keys ...string) any {
	t.Helper()
	var current any = report
	for _, key := range keys {
		obj, ok := current.(*pyjson.Object)
		if !ok {
			t.Fatalf("report path %v stops at %q", keys, key)
		}
		current = obj.GetOr(key, nil)
	}
	return current
}

func TestRecoveryRegeneratesRatherThanCopiesDerivedFtsMeta(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	source, output := filepath.Join(dir, "state.db"), filepath.Join(dir, "recovered.db")
	recoveryMakeSource(t, source)
	report, err := recoverSessionDatabase(context.Background(), source, output, dir, 1000, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, report, "complete") != true {
		t.Fatalf("not complete: %v", recoveryAt(t, report, "verification"))
	}
	rows, err := recoveryQueryAll(recoveryTestConn(t, output), "SELECT key FROM state_meta")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, row := range rows {
		keys[row[0].(string)] = true
	}
	if !keys["goal:recovery-session-0"] {
		t.Fatalf("user meta lost: %v", keys)
	}
	for _, derived := range []string{"fts_rebuild_high_water", "fts_rebuild_progress", "fts_tool_full_content_high_water"} {
		if keys[derived] {
			t.Fatalf("derived marker %s copied", derived)
		}
	}
}

func TestRecoveryWithoutDeliveryLedgerIsNotLossy(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	source, output := filepath.Join(dir, "state.db"), filepath.Join(dir, "recovered.db")
	recoveryMakeSource(t, source)
	report, err := recoverSessionDatabase(context.Background(), source, output, dir, 1000, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := recoveryAt(t, report, "copy", "delivery_obligations", "status"); got != "missing" {
		t.Fatalf("status %v", got)
	}
	counts := recoveryAt(t, report, "verification", "table_counts").(*pyjson.Object)
	if _, ok := counts.Get("delivery_obligations"); ok {
		t.Fatal("delivery_obligations counted")
	}
	if recoveryAt(t, report, "complete") != true || recoveryAt(t, report, "verified") != true {
		t.Fatalf("verdict %v %v", recoveryAt(t, report, "complete"), recoveryAt(t, report, "verified"))
	}
}

func TestRecoveryCopiesDeliveryObligations(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	source, output := filepath.Join(dir, "state.db"), filepath.Join(dir, "recovered.db")
	recoveryMakeSource(t, source)
	now := 1_720_000_000.0
	func() {
		conn, err := state.ConnectSQLite(source, state.SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := gateway.InitializeLedgerSchema(context.Background(), conn); err != nil {
			t.Fatal(err)
		}
		insert := `INSERT INTO delivery_obligations (obligation_id, session_key, platform, chat_id, thread_id,
			content, state, attempts, created_at, updated_at, owner_pid, owner_started_at, last_error, adapter_profile)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		recoveryTestExec(t, conn, insert, "ob-pending", "telegram:1:chat-1", "telegram", "chat-1", nil, "owed reply", "pending", 0, now, now, 4242, 99, nil, "default")
		recoveryTestExec(t, conn, insert, "ob-delivered", "telegram:1:chat-1", "telegram", "chat-1", nil, "already sent", "delivered", 1, now, now+1, nil, nil, nil, "default")
	}()
	inspection, err := inspectSessionDatabase(context.Background(), source, dir)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, inspection, "tables", "delivery_obligations", "available") != true || recoveryAt(t, inspection, "tables", "delivery_obligations", "rows") != int64(2) {
		t.Fatalf("inspection %v", recoveryAt(t, inspection, "tables", "delivery_obligations"))
	}
	report, err := recoverSessionDatabase(context.Background(), source, output, dir, 1000, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, report, "copy", "delivery_obligations", "status") != "complete" || recoveryAt(t, report, "copy", "delivery_obligations", "copied_rows") != int64(2) {
		t.Fatalf("copy %v", recoveryAt(t, report, "copy", "delivery_obligations"))
	}
	if recoveryAt(t, report, "verification", "table_counts", "delivery_obligations") != int64(2) {
		t.Fatalf("counts %v", recoveryAt(t, report, "verification", "table_counts"))
	}
	if recoveryAt(t, report, "complete") != true || recoveryAt(t, report, "verified") != true || recoveryAt(t, report, "installed") != false {
		t.Fatalf("verdict %v", recoveryAt(t, report, "verification"))
	}
	rows, err := recoveryQueryAll(recoveryTestConn(t, output), `SELECT obligation_id, state, content, owner_pid, adapter_profile
		FROM delivery_obligations ORDER BY obligation_id`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{"ob-delivered", "delivered", "already sent", nil, "default"}, {"ob-pending", "pending", "owed reply", int64(4242), "default"}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("recovered %v", rows)
	}
}

func TestLostAndFoundDirectCopyCreatesLazyDeliveryLedger(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	lfPath, output := filepath.Join(dir, "lost_and_found.db"), filepath.Join(dir, "rebuilt.db")
	now := 1_720_000_000.0
	lf := recoveryTestConn(t, lfPath)
	if err := gateway.InitializeLedgerSchema(context.Background(), lf); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO delivery_obligations (obligation_id, session_key, platform, chat_id, thread_id,
		content, state, attempts, created_at, updated_at, owner_pid, owner_started_at, last_error, adapter_profile)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	recoveryTestExec(t, lf, insert, "ob-1", "k", "telegram", "chat-1", nil, "one", "pending", 0, now, now, nil, nil, nil, "default")
	recoveryTestExec(t, lf, insert, "ob-2", "k", "telegram", "chat-1", nil, "two", "failed", 3, now, now, nil, nil, "boom", "default")
	db, err := state.NewSessionDB(context.Background(), output, false)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	dest := recoveryTestConn(t, output)
	if rows, err := recoveryQueryAll(dest, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='delivery_obligations'"); err != nil || len(rows) != 0 {
		t.Fatalf("ledger exists before the copy: %v %v", rows, err)
	}
	copied, err := lfCopyDirectTables(context.Background(), lf, dest)
	if err != nil {
		t.Fatal(err)
	}
	if copied.GetOr("delivery_obligations", nil) != int64(2) {
		t.Fatalf("copied %v", copied)
	}
	rows, err := recoveryQueryAll(dest, "SELECT obligation_id, state, last_error FROM delivery_obligations ORDER BY obligation_id")
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]any{{"ob-1", "pending", nil}, {"ob-2", "failed", "boom"}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows %v", rows)
	}
}

func TestPartialRecoveryKeepsMessagesWhenSessionsAreUnsalvageable(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	source, output := filepath.Join(dir, "sessions-destroyed.db"), filepath.Join(dir, "sessions-destroyed-recovered.db")
	perSession := map[string]int{"doomed-session-a": 40, "doomed-session-b": 35, "doomed-session-c": 45}
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, source, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"doomed-session-a", "doomed-session-b", "doomed-session-c"} {
		if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Cwd: "/tmp/" + sid}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < perSession[sid]; i++ {
			if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": "user", "content": fmt.Sprintf("irreplaceable %s %d", sid, i)}, state.AppendGuards{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	db.Close()
	func() {
		conn, err := state.ConnectSQLite(source, state.SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		recoveryTestExec(t, conn, "DELETE FROM sessions")
	}()
	report, err := recoverSessionDatabase(ctx, source, output, dir, 16, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, report, "orphan_cleanup", "messages_removed") != int64(0) {
		t.Fatal("salvaged messages were deleted for lack of a session row")
	}
	if recoveryAt(t, report, "orphan_cleanup", "sessions_reconstructed") != int64(3) || recoveryAt(t, report, "orphan_cleanup", "messages_retained") != int64(120) {
		t.Fatalf("cleanup %v", recoveryAt(t, report, "orphan_cleanup"))
	}
	verify := recoveryTestConn(t, output)
	sessions, err := recoveryQueryAll(verify, "SELECT id, source, title, message_count FROM sessions ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	count, err := recoveryCountRows(verify, "messages")
	if err != nil || count != 120 {
		t.Fatalf("expected all 120 messages retained, got %d %v", count, err)
	}
	if len(sessions) != 3 {
		t.Fatalf("sessions %v", sessions)
	}
	titles := map[string]bool{}
	for _, row := range sessions {
		sid := row[0].(string)
		title := row[2].(string)
		if row[1] != "recovered" || !strings.HasPrefix(title, "[recovered ") || int(row[3].(int64)) != perSession[sid] {
			t.Fatalf("placeholder %v", row)
		}
		titles[title] = true
	}
	if len(titles) != 3 {
		t.Fatalf("titles collide %v", titles)
	}
	if recoveryAt(t, report, "verification", "loss_detected") != true || recoveryAt(t, report, "partial") != true || recoveryAt(t, report, "complete") != false {
		t.Fatalf("verdict %v", recoveryAt(t, report, "verification"))
	}
	found := false
	for _, w := range recoveryAt(t, report, "verification", "warnings").([]any) {
		if strings.Contains(w.(string), "reconstructed as placeholders") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings %v", recoveryAt(t, report, "verification", "warnings"))
	}
	if !reflect.DeepEqual(recoveryAt(t, report, "verification", "integrity_check"), []any{"ok"}) ||
		len(recoveryAt(t, report, "verification", "foreign_key_check").([]any)) != 0 {
		t.Fatalf("structure %v", recoveryAt(t, report, "verification"))
	}
	if recoveryAt(t, report, "verified") != true || recoveryAt(t, report, "installed") != false {
		t.Fatalf("verified %v", recoveryAt(t, report, "verification"))
	}
}

func TestRecoveryResetsUnparseableModelConfig(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	source, output := filepath.Join(dir, "state.db"), filepath.Join(dir, "recovered.db")
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "parent", Source: "telegram", SessionKey: "telegram:u:c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "parent", map[string]any{"role": "user", "content": "hello"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("parent", "session_reset"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "child", Source: "telegram", SessionKey: "telegram:u:c", ParentSessionID: "parent"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	recoveryTestExec(t, recoveryTestConn(t, source), "UPDATE sessions SET model_config = ? WHERE id = 'child'", `{"model": "sonnet", "cw`)
	report, err := recoverSessionDatabase(ctx, source, output, "", 1000, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, report, "verification", "healthy") != true {
		t.Fatalf("errors %v", recoveryAt(t, report, "verification", "errors"))
	}
	recovered, err := state.NewSessionDB(ctx, output, false)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.ReopenSession(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRepairsOutOfWindowTimestamps(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	source, output := filepath.Join(dir, "state.db"), filepath.Join(dir, "recovered.db")
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three"} {
		if _, err := db.AppendMessage(ctx, "s", map[string]any{"role": "user", "content": text}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	raw := recoveryTestConn(t, source)
	stampRows, err := recoveryQueryAll(raw, "SELECT timestamp FROM messages WHERE session_id = 's' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	middle, _, err := recoveryQueryOne(raw, "SELECT id FROM messages WHERE session_id = 's' ORDER BY id LIMIT 1 OFFSET 1")
	if err != nil {
		t.Fatal(err)
	}
	recoveryTestExec(t, raw, "UPDATE messages SET timestamp = 5.4905047707024164e+246 WHERE id = ?", middle[0])
	recoveryTestExec(t, raw, "UPDATE sessions SET started_at = 'garbage', ended_at = 1e300, last_activity_at = -5 WHERE id = 's'")
	raw.Close()
	report, err := recoverSessionDatabase(ctx, source, output, "", 1000, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, report, "verification", "healthy") != true {
		t.Fatalf("errors %v", recoveryAt(t, report, "verification", "errors"))
	}
	conn := recoveryTestConn(t, output)
	rows, err := recoveryQueryAll(conn, "SELECT timestamp FROM messages WHERE session_id = 's' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var messages []float64
	for _, row := range rows {
		f, ok := row[0].(float64)
		if !ok || f < state.EpochMin || f > state.EpochMax {
			t.Fatalf("untrusted timestamp %v", row[0])
		}
		messages = append(messages, f)
	}
	if messages[0] != stampRows[0][0] || messages[2] != stampRows[2][0] || !(messages[0] <= messages[1] && messages[1] <= messages[2]) {
		t.Fatalf("messages %v stamps %v", messages, stampRows)
	}
	session, _, err := recoveryQueryOne(conn, "SELECT started_at, ended_at, last_activity_at FROM sessions WHERE id = 's'")
	if err != nil {
		t.Fatal(err)
	}
	if session[0] != messages[0] || session[1] != nil || session[2] != nil {
		t.Fatalf("session %v", session)
	}
}

func TestRecoverCarriesMessageIdentityColumns(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	source, output := filepath.Join(dir, "source.db"), filepath.Join(dir, "recovered.db")
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cli", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "s", map[string]any{"role": "user", "content": "q"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	absorbed := `["` + strings.Repeat("b", 32) + `"]`
	recoveryTestExec(t, recoveryTestConn(t, source), "UPDATE messages SET absorbed_message_uids = ? WHERE content = 'q'", absorbed)
	if db, err = state.NewSessionDB(ctx, source, false); err != nil {
		t.Fatal(err)
	}
	toolCalls := []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "t", "arguments": "{}"}}}
	if _, err := db.AppendMessage(ctx, "s", map[string]any{"role": "assistant", "content": "", "tool_calls": toolCalls}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "s", map[string]any{"role": "tool", "content": "r", "tool_call_id": "call_1", "tool_name": "t"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	query := "SELECT role, message_uid, absorbed_message_uids, tool_call_uids, tool_call_uid FROM messages WHERE session_id = 's' ORDER BY id"
	expected, err := recoveryQueryAll(recoveryTestConn(t, source), query)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range expected {
		if uid, _ := row[1].(string); len(uid) != 32 {
			t.Fatalf("message uid %v", row[1])
		}
	}
	report, err := recoverSessionDatabase(ctx, source, output, dir, 1000, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, report, "complete") != true {
		t.Fatalf("verification %v", recoveryAt(t, report, "verification"))
	}
	got, err := recoveryQueryAll(recoveryTestConn(t, output), query)
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("got %v want %v err %v", got, expected, err)
	}
	recovered, err := state.NewSessionDB(ctx, output, false)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	restored, err := recovered.GetMessagesAsConversation(ctx, "s", state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range restored {
		if m["message_uid"] != expected[i][1] {
			t.Fatalf("message %d uid %v", i, m["message_uid"])
		}
	}
	if !reflect.DeepEqual(fmt.Sprint(restored[0]["_absorbed_message_uids"]), fmt.Sprint([]any{strings.Repeat("b", 32)})) {
		t.Fatalf("absorbed %v", restored[0]["_absorbed_message_uids"])
	}
	var callUID any
	switch uids := restored[1]["_tool_call_uids"].(type) {
	case map[string]any:
		callUID = uids["call_1"]
	case map[string]string:
		callUID = uids["call_1"]
	case *pyjson.Object:
		callUID = uids.GetOr("call_1", nil)
	case state.PyObject:
		for _, kv := range uids {
			if kv.Key == "call_1" {
				callUID = kv.Value
			}
		}
	}
	if callUID == nil || restored[2]["_tool_call_uid"] != callUID {
		t.Fatalf("tool call uids %v %v", restored[1]["_tool_call_uids"], restored[2]["_tool_call_uid"])
	}
}

func TestRecoveryFlagsDeliveryObligationCountMismatchAsLoss(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	source, output := filepath.Join(dir, "state.db"), filepath.Join(dir, "recovered.db")
	recoveryMakeSource(t, source)
	now := 1_720_000_000.0
	func() {
		conn, err := state.ConnectSQLite(source, state.SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := gateway.InitializeLedgerSchema(context.Background(), conn); err != nil {
			t.Fatal(err)
		}
		insert := `INSERT INTO delivery_obligations (obligation_id, session_key, platform, chat_id, thread_id,
			content, state, attempts, created_at, updated_at, owner_pid, owner_started_at, last_error, adapter_profile)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		recoveryTestExec(t, conn, insert, "ob-a", "k", "telegram", "chat-1", nil, "a", "pending", 0, now, now, nil, nil, nil, "default")
		recoveryTestExec(t, conn, insert, "ob-b", "k", "telegram", "chat-1", nil, "b", "pending", 0, now, now, nil, nil, nil, "default")
	}()
	realInit := recoveryAuxiliarySchemas["delivery_obligations"]
	recoveryAuxiliarySchemas["delivery_obligations"] = func(ctx context.Context, conn state.SQLiteConn) error {
		if err := realInit(ctx, conn); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `CREATE TRIGGER drop_ob_b BEFORE INSERT ON delivery_obligations
			WHEN NEW.obligation_id = 'ob-b' BEGIN SELECT RAISE(IGNORE); END`)
		return err
	}
	t.Cleanup(func() { recoveryAuxiliarySchemas["delivery_obligations"] = realInit })
	report, err := recoverSessionDatabase(context.Background(), source, output, dir, 1000, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if recoveryAt(t, report, "verification", "table_counts", "delivery_obligations") != int64(1) || recoveryAt(t, report, "complete") != false {
		t.Fatalf("verification %v", recoveryAt(t, report, "verification"))
	}
	found := false
	for _, e := range recoveryAt(t, report, "verification", "errors").([]any) {
		found = found || strings.Contains(e.(string), "delivery_obligations count is 1, expected 2")
	}
	if !found {
		t.Fatalf("errors %v", recoveryAt(t, report, "verification", "errors"))
	}
}

func TestPartialRecoverySkipsPhantomRowRejectedByDestinationSchema(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	dir := t.TempDir()
	source, output := filepath.Join(dir, "phantom-state.db"), filepath.Join(dir, "phantom-recovered.db")
	recoveryMakeSource(t, source)
	func() {
		conn, err := state.ConnectSQLite(source, state.SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		recoveryTestExec(t, conn, "PRAGMA writable_schema=ON")
		recoveryTestExec(t, conn, "UPDATE sqlite_master SET sql = replace(sql, 'started_at REAL NOT NULL', 'started_at REAL') "+
			"WHERE type = 'table' AND name = 'sessions'")
		row, _, err := recoveryQueryOne(conn, "PRAGMA schema_version")
		if err != nil {
			t.Fatal(err)
		}
		recoveryTestExec(t, conn, fmt.Sprintf("PRAGMA schema_version=%d", row[0].(int64)+1))
		recoveryTestExec(t, conn, "PRAGMA writable_schema=OFF")
	}()
	func() {
		conn, err := state.ConnectSQLite(source, state.SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		recoveryTestExec(t, conn, "INSERT INTO sessions (id, source, started_at, title) VALUES ('phantom', 'cli', NULL, 'Phantom')")
		if n, err := recoveryCountRows(conn, "sessions"); err != nil || n != 4 {
			t.Fatalf("sessions %d %v", n, err)
		}
	}()
	report, err := recoverSessionDatabase(context.Background(), source, output, dir, 16, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	copied := recoveryAt(t, report, "copy", "sessions").(*pyjson.Object)
	if copied.GetOr("status", nil) != "partial" || copied.GetOr("copied_rows", nil) != int64(3) || copied.GetOr("destination_rejected_rows", nil) != int64(1) {
		t.Fatalf("copied %v", copied)
	}
	var errs []any
	for _, item := range copied.GetOr("skipped_rowid_ranges", nil).([]any) {
		errs = append(errs, item.(*pyjson.Object).GetOr("error", nil))
	}
	if !reflect.DeepEqual(errs, []any{"destination constraint rejected row: NOT NULL constraint failed: sessions.started_at"}) {
		t.Fatalf("skipped %v", errs)
	}
	if recoveryAt(t, report, "verified") != true {
		t.Fatalf("verification %v", recoveryAt(t, report, "verification"))
	}
	conn := recoveryTestConn(t, output)
	if row, _, err := recoveryQueryOne(conn, "SELECT count(*) FROM sessions WHERE id = 'phantom'"); err != nil || row[0] != int64(0) {
		t.Fatalf("phantom %v %v", row, err)
	}
	if n, err := recoveryCountRows(conn, "messages"); err != nil || n != 21 {
		t.Fatalf("messages %d %v", n, err)
	}
}

// Sessions with a large system prompt and one message each vacuumed into one file, returning the sessions root page
func recoveryMakeManySessionsSource(t *testing.T, path string, count int) int64 {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < count; n++ {
		sid := fmt.Sprintf("partial-session-%04d", n)
		prompt := fmt.Sprintf("session payload %04d ", n) + strings.Repeat(string(rune(65+n%26)), 1500)
		if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Cwd: "/tmp/" + sid, SystemPrompt: prompt}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": "user", "content": fmt.Sprintf("message %d", n)}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	conn := recoveryTestConn(t, path)
	for _, stmt := range []string{"PRAGMA wal_checkpoint(TRUNCATE)", "PRAGMA journal_mode=DELETE", "VACUUM"} {
		recoveryTestExec(t, conn, stmt)
	}
	row, ok, err := recoveryQueryOne(conn, "SELECT rootpage FROM sqlite_master WHERE type = 'table' AND name = 'sessions'")
	if err != nil || !ok {
		t.Fatalf("sessions root %v", err)
	}
	conn.Close()
	return row[0].(int64)
}

// Page size and leaf pages of a table b-tree, walked from its root in key order
func recoveryBtreeLeafPages(t *testing.T, path string, root int64) (int64, []int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pageSize := int64(data[16])<<8 | int64(data[17])
	if pageSize == 1 {
		pageSize = 65536
	}
	var leaves []int64
	visited := map[int64]bool{}
	var visit func(page int64)
	visit = func(page int64) {
		if visited[page] {
			return
		}
		visited[page] = true
		start := (page - 1) * pageSize
		header := start
		if page == 1 {
			header += 100
		}
		pageType := data[header]
		cells := int64(data[header+3])<<8 | int64(data[header+4])
		if pageType == 0x0A || pageType == 0x0D {
			leaves = append(leaves, page)
			return
		}
		if pageType != 0x02 && pageType != 0x05 {
			t.Fatalf("unexpected table b-tree page type %#x on page %d", pageType, page)
		}
		for i := int64(0); i < cells; i++ {
			pointer := header + 12 + i*2
			cell := start + (int64(data[pointer])<<8 | int64(data[pointer+1]))
			visit(int64(data[cell])<<24 | int64(data[cell+1])<<16 | int64(data[cell+2])<<8 | int64(data[cell+3]))
		}
		visit(int64(data[header+8])<<24 | int64(data[header+9])<<16 | int64(data[header+10])<<8 | int64(data[header+11]))
	}
	visit(root)
	return pageSize, leaves
}

func TestSalvageBoundsDamagedLowEdgeFromTheAggregateNotTheInt64Domain(t *testing.T) {
	recoveryWithoutCJKExtension(t)
	source := filepath.Join(t.TempDir(), "low-edge.db")
	root := recoveryMakeManySessionsSource(t, source, 180)
	pageSize, leaves := recoveryBtreeLeafPages(t, source, root)
	if len(leaves) < 3 {
		t.Fatalf("leaves %v", leaves)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	header := (leaves[0] - 1) * pageSize
	if data[header] != 0x0D {
		t.Fatalf("first leaf type %#x", data[header])
	}
	data[header+3], data[header+4] = 0xff, 0xff
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatal(err)
	}
	conn := recoveryTestConn(t, source)
	if _, _, err := recoveryQueryOne(conn, `SELECT rowid FROM "sessions" ORDER BY rowid ASC LIMIT 1`); !isSQLiteDatabaseError(err) {
		t.Fatalf("low edge still readable: %v", err)
	}
	bounds, err := recoverySalvageRowidBounds(conn, "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if bounds.GetOr("low", nil) != int64(1) || bounds.GetOr("high", nil) != int64(180) || len(bounds.GetOr("fallback_edges", nil).([]any)) != 0 {
		t.Fatalf("bounds %v", bounds)
	}
	destination := recoveryTestConn(t, filepath.Join(t.TempDir(), "dest.db"))
	recoveryTestExec(t, destination, "CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT, started_at REAL)")
	rows := int64(180)
	result, err := recoveryCopyTableSalvage(conn, destination, "sessions", 16, nil, &rows, "INSERT", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.GetOr("query_limit_reached", nil) != false || recoveryCounter(result, "range_queries") >= 200 || recoveryCounter(result, "copied_rows") < 120 {
		t.Fatalf("result %v", result)
	}
}
