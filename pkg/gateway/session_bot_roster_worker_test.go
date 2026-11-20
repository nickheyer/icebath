package gateway

import (
	"context"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Operator reader of the alpha profile's roster rows
var rosterWorkerReader = Principal{Subject: "uid:desk", ProfileID: "alpha", Capabilities: map[string]bool{"session:operator": true},
	TransportID: "transport:test"}

// Stores a session row with a fixed activity stamp so the newest worker is decided by the case
func storeRosterSession(t *testing.T, ctx context.Context, db *state.SessionDB, sid, source, title string, lastActive, hidden, archived int64) {
	t.Helper()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: source}); err != nil {
		t.Fatal(err)
	}
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET title=?, last_activity_at=?, started_at=?, hidden=?, archived=? WHERE id=?",
			title, lastActive, lastActive, hidden, archived, sid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Worker summary fields of a roster read as a plain mapping
func rosterWorker(t *testing.T, fields map[string]any) map[string]any {
	t.Helper()
	worker, _ := pyjson.Plain(fields["worker_session"]).(map[string]any)
	return worker
}

func TestTheNewestLiveWorkerIsReportedWithTheContractFields(t *testing.T) {
	ctx, db := newTestStateDB(t)
	for _, row := range []struct {
		sid, source, title       string
		active, hidden, archived int64
	}{
		{"work-old", "kanban", "Task 4", 1000, 0, 0}, {"work-new", "kanban", "Task 9", 3000, 0, 0},
		{"tool-run", "tool", "Subagent", 2000, 0, 0}, {"oneshot-run", "oneshot", "One-shot", 2500, 0, 0},
		{"chat", "gui", "Chat", 5000, 0, 0}, {"work-archived", "kanban", "Retired", 9000, 0, 1},
		{"work-hidden", "kanban", "Internal", 9500, 1, 0},
	} {
		storeRosterSession(t, ctx, db, row.sid, row.source, row.title, row.active, row.hidden, row.archived)
	}
	fields, err := readBotSessions(ctx, db, "uid:desk", false)
	if err != nil {
		t.Fatal(err)
	}
	worker := rosterWorker(t, fields)
	want := map[string]any{"id": "work-new", "source": "kanban", "title": "Task 9", "last_active": int64(3000)}
	if !pyjson.Equal(worker, want) {
		t.Fatalf("worker_session %v", worker)
	}
	if last, _ := pyjson.Plain(fields["last_session"]).(map[string]any); last != nil && last["id"] == "work-new" {
		t.Fatalf("the worker became the last session %v", last)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"canonical_session", "last_session", "worker_session"}) ||
		!slices.Equal(rosterSessionKeys, []string{"last_session", "worker_session", "canonical_session"}) {
		t.Fatalf("fields %v in row order %v", keys, rosterSessionKeys)
	}
}

func TestAProfileWithoutWorkersReportsNone(t *testing.T) {
	ctx, db := newTestStateDB(t)
	storeRosterSession(t, ctx, db, "chat", "gui", "Chat", 1000, 0, 0)
	fields, err := readBotSessions(ctx, db, "uid:desk", false)
	if err != nil {
		t.Fatal(err)
	}
	if fields["worker_session"] != nil {
		t.Fatalf("worker_session %v", fields["worker_session"])
	}
}

func TestTheOwnRowAndAHostScopedSiblingRowBothCarryTheWorker(t *testing.T) {
	ctx, db := newTestStateDB(t)
	storeRosterSession(t, ctx, db, "work-alpha", "kanban", "Task 1", 2000, 0, 0)
	a := &SessionAuthority{DB: db, ProfileID: "alpha"}
	own, err := botSessions(ctx, a, rosterWorkerReader)
	if err != nil {
		t.Fatal(err)
	}
	if worker := rosterWorker(t, own); worker["id"] != "work-alpha" || !pyjson.Equal(worker["last_active"], int64(2000)) {
		t.Fatalf("own worker %v", worker)
	}
	if worker := rosterWorker(t, siblingSessions(ctx, a, rosterWorkerReader)); worker["id"] != "work-alpha" {
		t.Fatalf("sibling worker %v", worker)
	}
	a.Retiring = true
	if fields := siblingSessions(ctx, a, rosterWorkerReader); len(fields) != 0 {
		t.Fatalf("retiring sibling read %v", fields)
	}
}
