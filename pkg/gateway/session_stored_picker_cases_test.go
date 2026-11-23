package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Adds one admission row of a status for the session
func storedPickerAdmission(t *testing.T, ctx context.Context, db *state.SessionDB, id, sid, status string) {
	t.Helper()
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO session_admissions(admission_id, request_id, principal_id,
			target_session_id, lineage_json, payload_json, payload_digest, intent, status, owner_epoch, generation)
			VALUES(?,?,?,?,?,?,?,?,?,1,1)`, id, "request-"+id, "owner", sid, `["`+sid+`"]`, `{"text":"q"}`, "digest-"+id, "queue", status)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Ids of the stored picker rows a native transport of the owner reads
func storedPickerIDs(t *testing.T, ctx context.Context, a *SessionAuthority) []any {
	t.Helper()
	actor := recoverActor()
	a.mu.Lock()
	a.nativeLegacyTransports = map[string]Principal{actor.TransportID: actor}
	a.mu.Unlock()
	listed, err := storedPickerSessions(ctx, a, actor, 200, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []any
	for _, row := range listed.GetOr("sessions", []any{}).([]any) {
		ids = append(ids, row.(*pyjson.Object).GetOr("id", nil))
	}
	return ids
}

// test_tui_detach_end_stamp.py test_a_closed_session_stays_in_the_stored_picker_after_a_restart, with its finished turn on record
func TestAClosedSessionWithAFinishedTurnStaysInTheStoredPickerAfterARestart(t *testing.T) {
	ctx, a, _ := newRecoverAuthority(t)
	ref, physical := seedRecoverSession(t, ctx, a, recoverActor(), recoverTUIParams(t.TempDir()))
	if _, err := a.DB.AppendMessage(ctx, physical, map[string]any{"role": "user", "content": "q"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	storedPickerAdmission(t, ctx, a.DB, "done", ref.SessionID, "terminal")
	if err := a.DB.EndSession(physical, "tui_close"); err != nil {
		t.Fatal(err)
	}
	restarted := recoverSecondOwner(t, ctx, a.DB)
	if ids := storedPickerIDs(t, ctx, restarted); !pyjson.Equal(ids, []any{ref.SessionID}) {
		t.Fatalf("stored picker %v", ids)
	}
	if _, live := restarted.live(ref.SessionID); live {
		t.Fatal("the listing restored the session")
	}
	storedPickerAdmission(t, ctx, a.DB, "waiting", ref.SessionID, "queued")
	if ids := storedPickerIDs(t, ctx, restarted); len(ids) != 0 {
		t.Fatalf("a session with queued input was offered as resumable %v", ids)
	}
}
