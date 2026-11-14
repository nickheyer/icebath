package state

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	sqlite3 "modernc.org/sqlite/lib"
)

// Fresh store holding session s whose canonical runtime schema exists
func runtimeStore(t *testing.T, path string) *SessionDB {
	t.Helper()
	db := mustSessionDB(t, path, false)
	if err := db.CreateSession(NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if scalarOf(t, db.conn, "SELECT name FROM sqlite_master WHERE name='runtime_epoch'") == nil {
		t.Fatal("canonical runtime schema missing")
	}
	return db
}

// Reason of a runtime refusal, failing the test for any other error
func refusalReason(t *testing.T, err error) string {
	t.Helper()
	var refusal *RuntimeStoreError
	if !errors.As(err, &refusal) {
		t.Fatalf("want a runtime refusal, got %v", err)
	}
	return refusal.Reason
}

func admissionText(text string) *pyjson.Object { return pyjson.ObjectOf("text", text) }

func TestAdmissionRetryClaimAndRestartAreFenced(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db := runtimeStore(t, path)
	defer func() { db.Close() }()
	ab, _ := AdmissionFingerprint("s", map[string]any{"a": int64(1), "b": int64(2)})
	ba, _ := AdmissionFingerprint("s", map[string]any{"b": int64(2), "a": int64(1)})
	if ab != ba {
		t.Fatal("fingerprint depends on key order")
	}
	if _, err := AdmissionFingerprint("s", map[string]any{"a": math.NaN()}); err == nil {
		t.Fatal("a NaN payload fingerprinted")
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "boot")
	if err != nil {
		t.Fatal(err)
	}
	args := SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "one", Payload: admissionText("hello")}
	first, err := db.AdmitSessionInput(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := db.AdmitSessionInput(ctx, args); err != nil || !pyjson.Equal(replay, first) {
		t.Fatalf("replay %v %v", replay, err)
	}
	changed, steer := args, args
	changed.Payload = admissionText("changed")
	steer.Intent = "steer"
	for _, change := range []SessionInput{changed, steer} {
		if _, err := db.AdmitSessionInput(ctx, change); refusalReason(t, err) != "admission_conflict" {
			t.Fatalf("change refused %v", err)
		}
	}
	two, other := args, args
	two.RequestID = "two"
	other.PrincipalID = "other"
	second, err := db.AdmitSessionInput(ctx, two)
	if err != nil {
		t.Fatal(err)
	}
	third, err := db.AdmitSessionInput(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	seq := func(row *pyjson.Object) int64 { n, _ := exactInt(rowValue(row, "seq")); return n }
	if !(seq(first) < seq(second) && seq(second) < seq(third)) {
		t.Fatalf("seq order %d %d %d", seq(first), seq(second), seq(third))
	}
	claimed, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "s"})
	if err != nil || !pyjson.Equal(rowValue(claimed, "admission_id"), rowValue(first, "admission_id")) || rowValue(claimed, "status") != "started" {
		t.Fatalf("claim %v %v", claimed, err)
	}
	if again, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "s"}); err != nil || again != nil {
		t.Fatalf("second claim %v %v", again, err)
	}
	claimedID := rowValue(claimed, "admission_id").(string)
	generation, _ := exactInt(rowValue(claimed, "generation"))
	if _, err := db.SettleSessionInput(ctx, SettleInput{Epoch: epoch, AdmissionID: claimedID, Generation: generation + 1, Outcome: "completed"}); refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
	if _, err := db.SettleSessionInput(ctx, SettleInput{Epoch: epoch, AdmissionID: claimedID, Generation: generation, Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	running, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "s"})
	if err != nil || !pyjson.Equal(rowValue(running, "admission_id"), rowValue(second, "admission_id")) {
		t.Fatalf("running %v %v", running, err)
	}
	runningGeneration, _ := exactInt(rowValue(running, "generation"))
	if runningGeneration <= generation {
		t.Fatalf("generation %d after %d", runningGeneration, generation)
	}
	db.Close()
	db = mustSessionDB(t, path, false)
	newEpoch, err := db.BeginRuntimeEpoch(ctx, "replacement")
	if err != nil || newEpoch <= epoch {
		t.Fatalf("new epoch %d %v", newEpoch, err)
	}
	for _, want := range []int64{1, 0} {
		if n, err := db.RecoverSessionInputs(ctx, newEpoch); err != nil || n != want {
			t.Fatalf("recovered %d want %d %v", n, want, err)
		}
	}
	secondID := rowValue(second, "admission_id").(string)
	if got, err := db.GetSessionAdmission(ctx, secondID); err != nil || rowValue(got, "status") != "unknown" {
		t.Fatalf("recovered row %v %v", got, err)
	}
	if _, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: newEpoch, SessionID: "s"}); refusalReason(t, err) != "unknown_execution" {
		t.Fatal(err)
	}
	if _, err := db.SettleSessionInput(ctx, SettleInput{Epoch: epoch, AdmissionID: secondID, Generation: runningGeneration, Outcome: "completed"}); refusalReason(t, err) != "stale_epoch" {
		t.Fatal(err)
	}
	cancelled, err := db.CancelSessionInput(ctx, newEpoch, rowValue(third, "admission_id").(string))
	if err != nil || rowValue(cancelled, "outcome") != "cancelled" {
		t.Fatalf("cancel %v %v", cancelled, err)
	}
	pending, err := db.ListSessionAdmissions(ctx, "s", true)
	if err != nil || len(pending) != 1 || rowValue(pending[0], "admission_id") != secondID {
		t.Fatalf("pending %v %v", pending, err)
	}
	if fk := scalarOf(t, db.conn, "PRAGMA foreign_keys"); fk != int64(1) {
		t.Fatalf("foreign keys %v", fk)
	}
	err = db.executeWrite(ctx, func(conn SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "DELETE FROM sessions WHERE id='s'")
		return err
	}, nil)
	if code, _ := sqliteErrorCode(err); code&0xff != sqlite3.SQLITE_CONSTRAINT {
		t.Fatalf("delete of an admitted session %v", err)
	}
}

func TestSqliteFailureNeverAcknowledgesOrPartiallyClaims(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db := runtimeStore(t, path)
	defer db.Close()
	epoch, err := db.BeginRuntimeEpoch(ctx, "boot")
	if err != nil {
		t.Fatal(err)
	}
	args := SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "one", Payload: admissionText("hello")}
	mustExecConn(t, db.conn, "CREATE TRIGGER refuse_admission BEFORE INSERT ON session_admissions BEGIN SELECT RAISE(ABORT, 'full fixture'); END")
	if _, err := db.AdmitSessionInput(ctx, args); err == nil || !strings.Contains(err.Error(), "full fixture") {
		t.Fatalf("refused insert %v", err)
	}
	if rows, err := db.ListSessionAdmissions(ctx, "s", true); err != nil || len(rows) != 0 {
		t.Fatalf("acknowledged %v %v", rows, err)
	}
	mustExecConn(t, db.conn, "DROP TRIGGER refuse_admission")
	row, err := db.AdmitSessionInput(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, db.conn, "CREATE TRIGGER refuse_claim BEFORE UPDATE ON session_admissions BEGIN SELECT RAISE(ABORT, 'claim fixture'); END")
	if _, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "s"}); err == nil || !strings.Contains(err.Error(), "claim fixture") {
		t.Fatalf("refused claim %v", err)
	}
	session, err := db.GetSession(ctx, "s")
	if err != nil || rowValue(session, "runtime_generation") != int64(0) {
		t.Fatalf("partial claim %v %v", session, err)
	}
	admissionID := rowValue(row, "admission_id").(string)
	if got, err := db.GetSessionAdmission(ctx, admissionID); err != nil || rowValue(got, "status") != "queued" {
		t.Fatalf("claimed row %v %v", got, err)
	}
	mustExecConn(t, db.conn, "DROP TRIGGER refuse_claim")
	peer, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerConn, err := peer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer peerConn.Close()
	var seen string
	if err := peerConn.QueryRowContext(ctx, "SELECT admission_id FROM session_admissions").Scan(&seen); err != nil || seen != admissionID {
		t.Fatalf("peer read %q %v", seen, err)
	}
	if _, err := peerConn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	saved := sessionWritePatienceS
	sessionWritePatienceS = 0
	busy := args
	busy.RequestID = "busy"
	_, err = db.AdmitSessionInput(ctx, busy)
	sessionWritePatienceS = saved
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("admission past a held writer %v", err)
	}
	if _, err := peerConn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.ListSessionAdmissions(ctx, "s", true); err != nil || len(rows) != 1 {
		t.Fatalf("admissions %v %v", rows, err)
	}
}

func TestSchemaReconcilesExistingSessionsWithoutLosingHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db := mustSessionDB(t, path, false)
	if err := db.CreateSession(NewSession{ID: "existing", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "existing", map[string]any{"role": "user", "content": "retained"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, legacy, "ALTER TABLE sessions DROP COLUMN runtime_generation")
	mustExec(t, legacy, "ALTER TABLE sessions DROP COLUMN runtime_revision")
	for _, table := range []string{"worker_receipts", "worker_executions", "session_admissions", "runtime_epoch"} {
		mustExec(t, legacy, "DROP TABLE "+table)
	}
	mustExec(t, legacy, "UPDATE schema_version SET version=?", SchemaVersion-1)
	legacy.Close()
	db = mustSessionDB(t, path, false)
	defer db.Close()
	session, err := db.GetSession(ctx, "existing")
	if err != nil || rowValue(session, "runtime_revision") != int64(0) || rowValue(session, "runtime_generation") != int64(0) {
		t.Fatalf("reconciled row %v %v", session, err)
	}
	messages, err := db.GetMessages(ctx, "existing", GetMessagesOptions{})
	if err != nil || len(messages) != 1 || rowValue(messages[0], "content") != "retained" {
		t.Fatalf("history %v %v", messages, err)
	}
	if v := scalarOf(t, db.conn, "SELECT version FROM schema_version"); v != int64(SchemaVersion) {
		t.Fatalf("schema version %v", v)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "boot")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "existing", RequestID: "r", Payload: admissionText("next")})
	if err != nil || rowValue(row, "status") != "queued" {
		t.Fatalf("admission %v %v", row, err)
	}
	if bad := scalarOf(t, db.conn, "PRAGMA foreign_key_check"); bad != nil {
		t.Fatalf("foreign key violations %v", bad)
	}
}

func TestRealSqliteFullAbortsAdmissionAndEpochFailureIsAtomic(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	defer db.Close()
	if err := db.CreateSession(NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "boot")
	if err != nil {
		t.Fatal(err)
	}
	mustExecConn(t, db.conn, "CREATE TRIGGER refuse_epoch BEFORE UPDATE ON runtime_epoch BEGIN SELECT RAISE(ABORT, 'epoch fixture'); END")
	if _, err := db.BeginRuntimeEpoch(ctx, "refused"); err == nil || !strings.Contains(err.Error(), "epoch fixture") {
		t.Fatalf("refused epoch %v", err)
	}
	row, err := fetchOne(ctx, db.conn, "SELECT epoch,instance_id FROM runtime_epoch")
	if err != nil || !pyjson.Equal(row, []any{epoch, "boot"}) {
		t.Fatalf("epoch row %v %v", row, err)
	}
	mustExecConn(t, db.conn, "DROP TRIGGER refuse_epoch")
	pages := scalarOf(t, db.conn, "PRAGMA page_count")
	mustExecConn(t, db.conn, "PRAGMA max_page_count="+pyStr(pages))
	args := SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "full", Payload: admissionText(strings.Repeat("x", 2*1024*1024))}
	_, err = db.AdmitSessionInput(ctx, args)
	if code, _ := sqliteErrorCode(err); code != sqlite3.SQLITE_FULL {
		t.Fatalf("full database admission %v", err)
	}
	if rows, err := db.ListSessionAdmissions(ctx, "s", true); err != nil || len(rows) != 0 {
		t.Fatalf("acknowledged %v %v", rows, err)
	}
	mustExecConn(t, db.conn, "PRAGMA max_page_count=2147483646")
	if row, err := db.AdmitSessionInput(ctx, args); err != nil || rowValue(row, "status") != "queued" {
		t.Fatalf("admission after space returned %v %v", row, err)
	}
}

// Local session committed the way commit_local_session reserves its row, route and receipt
func seedLocalSession(t *testing.T, db *SessionDB, cwd string) string {
	t.Helper()
	sid, err := LocalIdentity("profile", "human", "r")
	if err != nil {
		t.Fatal(err)
	}
	route := "local:" + sid
	origin := pyjson.ObjectOf("platform", "local", "chat_id", sid, "user_id", "human", "chat_type", "dm")
	entry := pyjson.ObjectOf("session_key", route, "session_id", sid, "created_at", "2026-11-06T00:00:00", "updated_at", "2026-11-06T00:00:00",
		"origin", origin, "platform", "local", "chat_type", "dm")
	policy := pyjson.ObjectOf("source", "cli", "cwd", cwd, "model", "m", "request_json", `{"source":"cli"}`)
	receipt := pyjson.ObjectOf("profile_id", "profile", "principal_id", "human", "request_id", "r", "session_id", sid,
		"route", route, "entry", entry, "policy", policy)
	entryJSON, _ := runtimeJSON(entry)
	originJSON, _ := runtimeJSON(origin)
	receiptJSON, _ := runtimeJSON(receipt)
	profileExec(t, db, `INSERT INTO sessions(id,source,created_source,user_id,session_key,chat_id,chat_type,model,cwd,origin_json,started_at)
        VALUES(?,'cli','cli','human',?,?,'dm','m',?,?,?)`, sid, route, sid, cwd, originJSON, now())
	profileExec(t, db, "INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES('',?,?,?)", route, entryJSON, now())
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", LocalPolicyPrefix+sid, receiptJSON)
	return sid
}

func TestResetLocalTargetAdvancesTheReceiptToItsResetChild(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	defer db.Close()
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedLocalSession(t, db, t.TempDir())
	if _, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: sid, RequestID: "queued", Payload: admissionText("queued")}); err != nil {
		t.Fatal(err)
	}
	entry := pyjson.ObjectOf("session_key", "local:"+sid, "session_id", "child", "is_fresh_reset", true)
	if err := db.ResetLocalTarget(ctx, epoch+1, sid, entry); refusalReason(t, err) != "stale_epoch" {
		t.Fatal(err)
	}
	if err := db.ResetLocalTarget(ctx, epoch, sid, entry); err != nil {
		t.Fatal(err)
	}
	child, err := db.GetSession(ctx, "child")
	if err != nil || rowValue(child, "parent_session_id") != sid || rowValue(child, "source") != "cli" || rowValue(child, "model") != "m" {
		t.Fatalf("reset child %v %v", child, err)
	}
	config, _ := pyjson.Loads(rowValue(child, "model_config").(string))
	if !pyjson.Equal(config, pyjson.ObjectOf("_reset_from", sid)) {
		t.Fatalf("child model config %v", config)
	}
	parent, err := db.GetSession(ctx, sid)
	if err != nil || rowValue(parent, "end_reason") != "session_reset" || rowValue(parent, "ended_at") == nil || rowValue(parent, "runtime_generation") != int64(1) {
		t.Fatalf("reset parent %v %v", parent, err)
	}
	if target, err := localPhysicalTarget(ctx, db.conn, sid); err != nil || target != "child" {
		t.Fatalf("physical target %q %v", target, err)
	}
	routed, err := routedEntry(ctx, db.conn, "local:"+sid, "child")
	if err != nil || routed == nil || rowValue(routed, "is_fresh_reset") != true {
		t.Fatalf("route entry %v %v", routed, err)
	}
	if pending, err := db.ListSessionAdmissions(ctx, sid, true); err != nil || len(pending) != 1 {
		t.Fatalf("queued follower %v %v", pending, err)
	}
	after, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: sid, RequestID: "after", Payload: admissionText("after")})
	if err != nil || rowValue(after, "status") != "queued" {
		t.Fatalf("admission after reset %v %v", after, err)
	}
}

func TestResetLocalTargetRefusesExecutingWorkAndCompressedParents(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	defer db.Close()
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedLocalSession(t, db, t.TempDir())
	if _, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: sid, RequestID: "head", Payload: admissionText("head")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: sid}); err != nil {
		t.Fatal(err)
	}
	entry := pyjson.ObjectOf("session_key", "local:"+sid, "session_id", "child")
	if err := db.ResetLocalTarget(ctx, epoch, sid, entry); refusalReason(t, err) != "session_busy" {
		t.Fatal(err)
	}
	if n := scalarOf(t, db.conn, "SELECT COUNT(*) FROM sessions WHERE id='child'"); n != int64(0) {
		t.Fatal("a refused reset published its child")
	}
	profileExec(t, db, "UPDATE sessions SET end_reason='compression', ended_at=? WHERE id=?", now(), sid)
	if err := db.ResetLocalTarget(ctx, epoch, sid, entry); refusalReason(t, err) != "admission_conflict" {
		t.Fatal(err)
	}
	if err := db.ResetLocalTarget(ctx, epoch, "missing", entry); refusalReason(t, err) != "admission_conflict" {
		t.Fatal(err)
	}
}

func TestRuntimeAdmissionExportReadsTheLiveRowAndRefusesAMissingOne(t *testing.T) {
	ctx := context.Background()
	db := runtimeStore(t, filepath.Join(t.TempDir(), "state.db"))
	if err := db.CreateSession(NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "owner", SessionID: "s", RequestID: "r", Payload: admissionText("hi")})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := row.Get("admission_id")
	err = db.ReadCtx(ctx, func(conn SQLiteConn) error {
		got, err := RuntimeAdmission(ctx, conn, id.(string))
		if err != nil {
			return err
		}
		if status, _ := got.Get("status"); status != "queued" {
			t.Fatalf("status = %v", status)
		}
		_, err = RuntimeAdmission(ctx, conn, "missing")
		var refusal *RuntimeStoreError
		if !errors.As(err, &refusal) || refusal.Reason != "not_found" {
			t.Fatalf("missing admission error = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
