package state

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// In memory store carrying the live schema, as the hermes cascade fixture builds it
func cascadeConn(t *testing.T) SQLiteConn {
	t.Helper()
	t.Setenv("ICEBATH_CJK_FTS", "off")
	conn, err := ConnectSQLite(":memory:", SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := ReconcileStateSchema(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

// Session row with an optional delegate marker, parent and user messages
func cascadeSession(t *testing.T, conn SQLiteConn, sid, delegateFrom, parent string, messages int) {
	t.Helper()
	ctx := context.Background()
	var modelConfig, parentArg any
	if delegateFrom != "" {
		text, err := pyDumps(pyjson.ObjectOf("_delegate_from", delegateFrom), pyJSONDefault)
		if err != nil {
			t.Fatal(err)
		}
		modelConfig = text
	}
	if parent != "" {
		parentArg = parent
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO sessions (id, source, started_at, parent_session_id, model_config) VALUES (?, 'cli', 0, ?, ?)",
		sid, parentArg, modelConfig); err != nil {
		t.Fatal(err)
	}
	for range messages {
		if _, err := conn.ExecContext(ctx, "INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, 'user', 'x', 0)", sid); err != nil {
			t.Fatal(err)
		}
	}
}

// Delegate children collected for the parents
func cascadeCollect(t *testing.T, conn SQLiteConn, parents ...string) []string {
	t.Helper()
	ids, err := collectDelegateChildIDs(context.Background(), conn, parents)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	return ids
}

// Count of a single value statement on a connection
func cascadeCount(t *testing.T, conn SQLiteConn, query string) int64 {
	t.Helper()
	return rawCount(t, conn, query)
}

func TestCollectsDelegateChildExcludesParent(t *testing.T) {
	conn := cascadeConn(t)
	cascadeSession(t, conn, "P", "", "", 0)
	cascadeSession(t, conn, "C", "P", "", 0)
	if got := cascadeCollect(t, conn, "P"); !reflect.DeepEqual(got, []string{"C"}) {
		t.Fatalf("collected %v", got)
	}
}

func TestMultilevelChainCollectsAllDescendants(t *testing.T) {
	conn := cascadeConn(t)
	cascadeSession(t, conn, "O", "", "", 0)
	cascadeSession(t, conn, "A", "O", "", 0)
	cascadeSession(t, conn, "B", "A", "", 0)
	if got := cascadeCollect(t, conn, "O"); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("collected %v", got)
	}
}

func TestParentSessionIdBranchWithMarkerCollected(t *testing.T) {
	conn := cascadeConn(t)
	cascadeSession(t, conn, "P", "", "", 0)
	cascadeSession(t, conn, "C", "something", "P", 0)
	if got := cascadeCollect(t, conn, "P"); !reflect.DeepEqual(got, []string{"C"}) {
		t.Fatalf("collected %v", got)
	}
}

func TestUntaggedChildNotCollected(t *testing.T) {
	conn := cascadeConn(t)
	cascadeSession(t, conn, "P", "", "", 0)
	cascadeSession(t, conn, "C", "", "P", 0)
	if got := cascadeCollect(t, conn, "P"); len(got) != 0 {
		t.Fatalf("collected %v", got)
	}
}

func TestCycleTerminatesAndExcludesParent(t *testing.T) {
	conn := cascadeConn(t)
	cascadeSession(t, conn, "A", "B", "", 0)
	cascadeSession(t, conn, "B", "A", "", 0)
	if got := cascadeCollect(t, conn, "A"); !reflect.DeepEqual(got, []string{"B"}) {
		t.Fatalf("collected %v", got)
	}
}

func TestCycleDoesNotDeleteParentOrItsMessages(t *testing.T) {
	installRetirementHooks(t)
	conn := cascadeConn(t)
	cascadeSession(t, conn, "A", "B", "", 3)
	cascadeSession(t, conn, "B", "A", "", 2)
	removed, err := deleteDelegateChildren(context.Background(), conn, []string{"A"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{"B"}) {
		t.Fatalf("removed %v", removed)
	}
	if cascadeCount(t, conn, "SELECT COUNT(*) FROM sessions WHERE id='A'") != 1 || cascadeCount(t, conn, "SELECT COUNT(*) FROM messages WHERE session_id='A'") != 3 {
		t.Fatal("parent A or its messages were deleted")
	}
	if cascadeCount(t, conn, "SELECT COUNT(*) FROM sessions WHERE id='B'") != 0 || cascadeCount(t, conn, "SELECT COUNT(*) FROM messages WHERE session_id='B'") != 0 {
		t.Fatal("delegate child B survived")
	}
}

// Whether a path exists on disk
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRemoveSessionFilesEscapesGlobMetacharacters(t *testing.T) {
	dir := t.TempDir()
	tricky, neighbour := "sess-[ab]-1", "sess-a-1"
	for _, sid := range []string{tricky, neighbour} {
		for _, name := range []string{sid + ".jsonl", "request_dump_" + sid + "_0.json"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	removeSessionFiles(dir, tricky)
	if fileExists(filepath.Join(dir, tricky+".jsonl")) || fileExists(filepath.Join(dir, "request_dump_"+tricky+"_0.json")) {
		t.Fatal("own files were not matched")
	}
	if !fileExists(filepath.Join(dir, neighbour+".jsonl")) || !fileExists(filepath.Join(dir, "request_dump_"+neighbour+"_0.json")) {
		t.Fatal("neighbour's files were removed")
	}
}

func TestRemoveSessionFilesRemovesLegacySessionPrefixSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "session_abc123.json")
	if err := os.WriteFile(snapshot, []byte(`{"messages": [{"content": "secret"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	removeSessionFiles(dir, "abc123")
	if fileExists(snapshot) {
		t.Fatal("legacy snapshot survived")
	}
}

func TestClassifySessionStatus(t *testing.T) {
	for _, c := range []struct {
		role         any
		hasToolCalls bool
		finishReason any
		expected     string
	}{
		{"assistant", false, "stop", SessionStatusComplete},
		{"assistant", false, nil, SessionStatusComplete},
		{"assistant", false, "length", SessionStatusComplete},
		{"assistant", true, "tool_calls", SessionStatusInterrupted},
		{"user", false, nil, SessionStatusInterrupted},
		{"tool", false, nil, SessionStatusInterrupted},
		{"assistant", false, "error", SessionStatusError},
		{"assistant", true, "error", SessionStatusError},
		{"user", false, "agent_error", SessionStatusError},
		{"system", false, nil, SessionStatusComplete},
		{nil, false, nil, SessionStatusComplete},
	} {
		if got := ClassifySessionStatus(c.role, c.hasToolCalls, c.finishReason); got != c.expected {
			t.Fatalf("%v %v %v classified %q", c.role, c.hasToolCalls, c.finishReason, got)
		}
	}
}

// Appends one message with its extra fields through the store
func statusAppend(t *testing.T, db *SessionDB, sid, role string, content any, extra map[string]any) {
	t.Helper()
	fields := map[string]any{"role": role, "content": content}
	for k, v := range extra {
		fields[k] = v
	}
	if _, err := db.AppendMessage(context.Background(), sid, fields, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionLifecycleStatusesShapes(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	call := []any{map[string]any{"id": "c1", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}
	for _, sid := range []string{"s_complete", "s_user_tail", "s_pending_tool", "s_tool_roundtrip", "s_tool_tail", "s_error", "s_empty"} {
		readSession(t, db, NewSession{ID: sid, Source: "cli"})
	}
	statusAppend(t, db, "s_complete", "user", "hi", nil)
	statusAppend(t, db, "s_complete", "assistant", "hello", map[string]any{"finish_reason": "stop"})
	statusAppend(t, db, "s_user_tail", "user", "are you there?", nil)
	statusAppend(t, db, "s_pending_tool", "user", "run it", nil)
	statusAppend(t, db, "s_pending_tool", "assistant", nil, map[string]any{"tool_calls": call, "finish_reason": "tool_calls"})
	statusAppend(t, db, "s_tool_roundtrip", "user", "run it", nil)
	statusAppend(t, db, "s_tool_roundtrip", "assistant", nil, map[string]any{"tool_calls": call, "finish_reason": "tool_calls"})
	statusAppend(t, db, "s_tool_roundtrip", "tool", "ok", map[string]any{"tool_call_id": "c1"})
	statusAppend(t, db, "s_tool_roundtrip", "assistant", "done", map[string]any{"finish_reason": "stop"})
	statusAppend(t, db, "s_tool_tail", "user", "run it", nil)
	statusAppend(t, db, "s_tool_tail", "assistant", nil, map[string]any{"tool_calls": []any{map[string]any{"id": "c2", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}, "finish_reason": "tool_calls"})
	statusAppend(t, db, "s_tool_tail", "tool", "ok", map[string]any{"tool_call_id": "c2"})
	statusAppend(t, db, "s_error", "user", "hi", nil)
	statusAppend(t, db, "s_error", "assistant", "boom", map[string]any{"finish_reason": "error"})
	statuses, err := db.SessionLifecycleStatuses(ctx, []string{"s_complete", "s_user_tail", "s_pending_tool", "s_tool_roundtrip", "s_tool_tail", "s_error", "s_empty"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"s_complete": SessionStatusComplete, "s_user_tail": SessionStatusInterrupted, "s_pending_tool": SessionStatusInterrupted,
		"s_tool_roundtrip": SessionStatusComplete, "s_tool_tail": SessionStatusInterrupted, "s_error": SessionStatusError,
		"s_empty": SessionStatusEmpty,
	}
	if !reflect.DeepEqual(statuses, want) {
		t.Fatalf("statuses %v", statuses)
	}
}

func TestSessionLifecycleStatusesEmptyInput(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	for _, ids := range [][]string{{}, {""}} {
		statuses, err := db.SessionLifecycleStatuses(ctx, ids)
		if err != nil || len(statuses) != 0 {
			t.Fatalf("%v -> %v %v", ids, statuses, err)
		}
	}
}

func TestSessionLifecycleStatusesUnknownId(t *testing.T) {
	db := usageDB(t)
	statuses, err := db.SessionLifecycleStatuses(context.Background(), []string{"nope"})
	if err != nil || !reflect.DeepEqual(statuses, map[string]string{"nope": SessionStatusEmpty}) {
		t.Fatalf("statuses %v %v", statuses, err)
	}
}

func TestDeleteSessionRemovesSessionAndMessages(t *testing.T) {
	ctx := context.Background()
	installRetirementHooks(t)
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "doomed", Source: "cli"})
	statusAppend(t, db, "doomed", "user", "hi", nil)
	statusAppend(t, db, "doomed", "assistant", "hello", map[string]any{"finish_reason": "stop"})
	deleted, err := db.DeleteSession(ctx, "doomed", DeleteSessionOptions{SessionsDir: filepath.Join(t.TempDir(), "sessions")})
	if err != nil || !deleted {
		t.Fatalf("delete %v %v", deleted, err)
	}
	if row, err := db.GetSession(ctx, "doomed"); err != nil || row != nil {
		t.Fatalf("session still readable %v %v", row, err)
	}
	remaining, err := db.readOne(ctx, "SELECT COUNT(*) AS n FROM messages WHERE session_id = ?", "doomed")
	if err != nil || !pyjson.Equal(rowValue(remaining, "n"), int64(0)) {
		t.Fatalf("messages remain %v %v", pyjson.Plain(remaining), err)
	}
	if deleted, err := db.DeleteSession(ctx, "doomed", DeleteSessionOptions{}); err != nil || deleted {
		t.Fatalf("second delete %v %v", deleted, err)
	}
}

func TestRepoRootIsTheKeyWhenKnown(t *testing.T) {
	key, ok := WorkspaceKey(map[string]any{"git_repo_root": "/www/app", "cwd": "/www/app/src", "git_branch": "feat"})
	if !ok || key != "/www/app" {
		t.Fatalf("key %q %v", key, ok)
	}
}

func TestFallsBackToCwdForNonGitSessions(t *testing.T) {
	for _, row := range []map[string]any{{"cwd": "/work/notes"}, {"git_repo_root": "", "cwd": "/work/notes"}} {
		if key, ok := WorkspaceKey(row); !ok || key != "/work/notes" {
			t.Fatalf("%v -> %q %v", row, key, ok)
		}
	}
}

func TestNoneWhenUnbound(t *testing.T) {
	for _, row := range []map[string]any{{}, {"cwd": "", "git_repo_root": ""}, {"cwd": "   "}} {
		if key, ok := WorkspaceKey(row); ok {
			t.Fatalf("%v -> %q", row, key)
		}
	}
}

func TestBranchDoesNotAffectTheKey(t *testing.T) {
	a, _ := WorkspaceKey(map[string]any{"git_repo_root": "/www/app", "git_branch": "main"})
	b, _ := WorkspaceKey(map[string]any{"git_repo_root": "/www/app", "git_branch": "feature-x"})
	if a != "/www/app" || b != "/www/app" {
		t.Fatalf("keys %q %q", a, b)
	}
}

func TestRepoRootWinsOverADifferingCwd(t *testing.T) {
	if key, ok := WorkspaceKey(map[string]any{"git_repo_root": "/www/app", "cwd": "/www/app/.worktrees/x"}); !ok || key != "/www/app" {
		t.Fatalf("key %q %v", key, ok)
	}
}

func TestPromoteAdvancesItToo(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	key := "agent:main:telegram:dm:777"
	readSession(t, db, NewSession{ID: "s-a", Source: "telegram", SessionKey: key})
	if err := db.EndSession("s-a", "agent_close"); err != nil {
		t.Fatal(err)
	}
	if gen, ok, err := db.LatestConversationBoundary(ctx, key, "telegram"); err != nil || ok {
		t.Fatalf("boundary before promote %d %v %v", gen, ok, err)
	}
	if !db.PromoteToSessionReset(ctx, "s-a", "session_reset") {
		t.Fatal("promote wrote nothing")
	}
	if gen, ok, err := db.LatestConversationBoundary(ctx, key, "telegram"); err != nil || !ok || gen != 1 {
		t.Fatalf("boundary after promote %d %v %v", gen, ok, err)
	}
}
