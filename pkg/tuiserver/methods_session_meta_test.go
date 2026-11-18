package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Column of a stored row, failing the case when the row is missing
func storedColumn(t *testing.T, db *state.DB, sid, column string) any {
	t.Helper()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil || row == nil {
		t.Fatalf("row %s: %v %v", sid, row, err)
	}
	return row.GetOr(column, nil)
}

func envelopeResult(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	result, isResult := envelope["result"].(map[string]any)
	if !isResult {
		t.Fatalf("envelope %v", envelope)
	}
	return result
}

// test_session_delete_rpc.py test_session_delete_requires_session_id
func TestSessionDeleteRequiresSessionID(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "kept", "tui", 1)
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.delete", "params": map[string]any{}})
	if failure, _ := resp["error"].(map[string]any); resumeErrCode(resp) != 4000 || failure["message"] != "invalid params for session.delete: session_id: field required" {
		t.Fatalf("resp %v", resp)
	}
	storedColumn(t, db, "kept", "id")
}

// test_session_delete_rpc.py test_session_delete_refuses_active_session
func TestSessionDeleteRefusesActiveSession(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "key-live", "tui", 1)
	tuihost.SetSessionRecord("live", tuihost.NewSession(map[string]any{"session_key": "key-live"}))
	t.Cleanup(func() { tuihost.SetSessionRecord("live", nil) })
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.delete", "params": map[string]any{"session_id": "key-live"}})
	if resumeErrCode(resp) != 4023 {
		t.Fatalf("resp %v", resp)
	}
	storedColumn(t, db, "key-live", "id")
}

// test_session_delete_rpc.py test_session_delete_returns_4007_when_missing
func TestSessionDeleteReturns4007WhenMissing(t *testing.T) {
	listingDB(t)
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.delete", "params": map[string]any{"session_id": "ghost"}})
	if resumeErrCode(resp) != 4007 {
		t.Fatalf("resp %v", resp)
	}
}

// test_session_delete_rpc.py test_session_delete_propagates_db_exception
func TestSessionDeletePropagatesDBException(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "x", "tui", 1)
	reaperExec(t, db, "CREATE TRIGGER refuse_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT, 'disk full'); END")
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.delete", "params": map[string]any{"session_id": "x"}})
	errObj, _ := resp["error"].(map[string]any)
	if resumeErrCode(resp) != 5036 || !strings.Contains(strOr(errObj["message"]), "disk full") {
		t.Fatalf("resp %v", resp)
	}
}

// test_session_delete_rpc.py test_session_delete_success_returns_deleted_id
func TestSessionDeleteSuccessReturnsDeletedID(t *testing.T) {
	db := listingDB(t)
	seedCounted(t, db, "old-1", "tui", 1)
	sessionsDir := filepath.Join(home.Dir(context.Background()), "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(sessionsDir, "old-1.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := handleRequest(context.Background(), map[string]any{"id": "1", "method": "session.delete", "params": map[string]any{"session_id": "old-1"}})
	result := envelopeResult(t, resp)
	if len(result) != 1 || result["deleted"] != "old-1" {
		t.Fatalf("result %v", result)
	}
	if row, _ := db.GetSession(context.Background(), "old-1"); row != nil {
		t.Fatalf("row kept %v", row)
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("transcript in the sessions dir kept: %v", err)
	}
}

// A session scoped RPC against a reaped runtime id logs the method name and the id it rejected
func TestSessionScopedRejectionLogsMethodName(t *testing.T) {
	listingDB(t)
	capture := captureLogs(t)
	resp := handleRequest(context.Background(), map[string]any{"id": "r1", "method": "session.usage", "params": map[string]any{"session_id": "gone-sid"}})
	if resp["id"] != "r1" || resumeErrCode(resp) != 4001 {
		t.Fatalf("resp %v", resp)
	}
	logged := capture.take("session-scoped RPC rejected")
	if len(logged) != 1 || !strings.Contains(logged[0], "gone-sid") || !strings.Contains(logged[0], "method=session.usage") {
		t.Fatalf("logged %v", logged)
	}
}

// The pool path of a long session scoped RPC names the method in its rejection warning too
func TestPooledSessionScopedRejectionLogsMethodName(t *testing.T) {
	listingDB(t)
	capture := captureLogs(t)
	if resp := Dispatch(context.Background(), map[string]any{"id": "r2", "method": "session.usage", "params": map[string]any{"session_id": "gone-pool"}}, nil); resp != nil {
		t.Fatalf("long handler answered inline %v", resp)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		capture.mu.Lock()
		var hit string
		for _, msg := range capture.msgs {
			if strings.Contains(msg, "gone-pool") {
				hit = msg
			}
		}
		capture.mu.Unlock()
		if hit != "" {
			if !strings.Contains(hit, "method=session.usage") {
				t.Fatalf("logged %q", hit)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no rejection warning")
}
