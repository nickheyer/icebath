package state

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Admits one queued input and returns its admission id
func admitQueued(t *testing.T, db *SessionDB, epoch int64, principal, sid, request string, payload *pyjson.Object) string {
	t.Helper()
	row, err := db.AdmitSessionInput(context.Background(), SessionInput{Epoch: epoch, PrincipalID: principal, SessionID: sid, RequestID: request, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return objStr(row, "admission_id")
}

func TestRehomeQueuedInputsKeepsFIFOAndStopsBehindAnUnknownHead(t *testing.T) {
	ctx := context.Background()
	db := runtimeStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer db.Close()
	readSession(t, db, NewSession{ID: "n", Source: "test"})
	epoch, err := db.BeginRuntimeEpoch(ctx, "boot")
	if err != nil {
		t.Fatal(err)
	}
	first := admitQueued(t, db, epoch, "telegram:1", "s", "a", admissionText("one"))
	admitQueued(t, db, epoch, "cli:me", "s", "b", admissionText("two"))
	third := admitQueued(t, db, epoch, "telegram:2", "s", "c", admissionText("three"))
	moved, err := db.RehomeQueuedInputs(ctx, epoch, "s", "n", "telegram:")
	if err != nil || !slices.Equal(moved, []string{first, third}) {
		t.Fatalf("moved %v %v", moved, err)
	}
	row, _ := db.GetSessionAdmission(ctx, first)
	if objStr(row, "target_session_id") != "n" {
		t.Fatalf("row %v", row)
	}
	want, _ := AdmissionFingerprint("n", map[string]any{"input": admissionText("one"), "intent": "queue"})
	if digest := scalarOf(t, db.conn, "SELECT payload_digest FROM session_admissions WHERE admission_id=?", first); digest != want {
		t.Fatalf("digest %v", digest)
	}
	if rev := scalarOf(t, db.conn, "SELECT runtime_revision FROM sessions WHERE id='n'"); rev != int64(1) {
		t.Fatalf("revision %v", rev)
	}
	again, err := db.RehomeQueuedInputs(ctx, epoch, "s", "n", "telegram:")
	if err != nil || len(again) != 0 {
		t.Fatalf("second rehome %v %v", again, err)
	}
	fourth := admitQueued(t, db, epoch, "telegram:3", "s", "d", admissionText("four"))
	if _, err := db.conn.ExecContext(ctx, "UPDATE session_admissions SET status='unknown' WHERE request_id='b'"); err != nil {
		t.Fatal(err)
	}
	if moved, err := db.RehomeQueuedInputs(ctx, epoch, "s", "n", "telegram:"); err != nil || len(moved) != 0 {
		t.Fatalf("moved behind unknown head %v %v", moved, err)
	}
	if row, _ := db.GetSessionAdmission(ctx, fourth); objStr(row, "target_session_id") != "s" {
		t.Fatalf("row moved behind unknown head %v", row)
	}
	if _, err := db.RehomeQueuedInputs(ctx, epoch, "s", "missing", "telegram:"); refusalReason(t, err) != "not_found" {
		t.Fatalf("missing target %v", err)
	}
}

func TestRetargetLocalAutomationRewritesOnlyStaleDescriptors(t *testing.T) {
	ctx := context.Background()
	db := runtimeStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer db.Close()
	readSession(t, db, NewSession{ID: "fresh", Source: "test"})
	epoch, _ := db.BeginRuntimeEpoch(ctx, "boot")
	automation := func(target string) *pyjson.Object {
		return pyjson.ObjectOf("text", "notice", "local_automation_v1", pyjson.ObjectOf("target", target, "kind", "peer"))
	}
	stale := admitQueued(t, db, epoch, "bot", "s", "a", automation("old"))
	admitQueued(t, db, epoch, "bot", "s", "b", automation("other"))
	admitQueued(t, db, epoch, "bot", "s", "c", admissionText("plain"))
	admitQueued(t, db, epoch, "human", "s", "d", automation("old"))
	moved, err := db.RetargetLocalAutomation(ctx, epoch, "s", "bot", []string{"old"}, "fresh")
	if err != nil || !slices.Equal(moved, []string{stale}) {
		t.Fatalf("moved %v %v", moved, err)
	}
	payload := scalarOf(t, db.conn, "SELECT payload_json FROM session_admissions WHERE admission_id=?", stale)
	if payload != `{"local_automation_v1":{"kind":"peer","target":"fresh"},"text":"notice"}` {
		t.Fatalf("payload %v", payload)
	}
	want, _ := AdmissionFingerprint("s", map[string]any{"input": automation("fresh"), "intent": "queue"})
	if digest := scalarOf(t, db.conn, "SELECT payload_digest FROM session_admissions WHERE admission_id=?", stale); digest != want {
		t.Fatalf("digest %v", digest)
	}
	if row, _ := db.GetSessionAdmission(ctx, stale); objStr(row, "target_session_id") != "s" {
		t.Fatalf("owner moved %v", row)
	}
	admitQueued(t, db, epoch, "bot", "s", "e", pyjson.ObjectOf("local_automation_v1", pyjson.ObjectOf("target", []any{"old"})))
	if _, err := db.RetargetLocalAutomation(ctx, epoch, "s", "bot", []string{"other"}, "fresh"); err == nil {
		t.Fatal("unhashable target accepted")
	}
}

func TestReplacementBindingRetiresTheQueuedRowItNames(t *testing.T) {
	ctx := context.Background()
	db := runtimeStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "boot")
	replaced := admitQueued(t, db, epoch, "human", "s", "a", admissionText("draft"))
	binding, err := ReplacementBinding("human", "s", "b")
	if err != nil || len(binding) != len(ReplacementPrefix)+64 {
		t.Fatalf("binding %q %v", binding, err)
	}
	write := func(fn func(conn SQLiteConn) error) error { return db.executeWrite(ctx, fn, nil) }
	if err := write(func(conn SQLiteConn) error { return RetireReplacedInput(ctx, conn, replaced, binding) }); err != nil {
		t.Fatal(err)
	}
	row, _ := db.GetSessionAdmission(ctx, replaced)
	if objStr(row, "status") != "terminal" || objStr(row, "outcome") != "cancelled" {
		t.Fatalf("row %v", row)
	}
	if err := write(func(conn SQLiteConn) error { return CheckReplacedInput(ctx, conn, replaced, binding) }); err != nil {
		t.Fatal(err)
	}
	if err := write(func(conn SQLiteConn) error { return CheckReplacedInput(ctx, conn, "other", binding) }); refusalReason(t, err) != "admission_conflict" {
		t.Fatal(err)
	}
	if err := write(func(conn SQLiteConn) error { return RetireReplacedInput(ctx, conn, replaced, binding) }); refusalReason(t, err) != "admission_started" {
		t.Fatal(err)
	}
}

func TestMutationReceiptReplaysOnlyTheExactRequest(t *testing.T) {
	ctx := context.Background()
	db := runtimeStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer db.Close()
	gen := int64(2)
	key, digest, err := MutationReceiptKey("human", "s", "r", "title", pyjson.ObjectOf("title", "x"), 1, &gen)
	if err != nil {
		t.Fatal(err)
	}
	_, other, _ := MutationReceiptKey("human", "s", "r", "title", pyjson.ObjectOf("title", "x"), 1, nil)
	if other == digest {
		t.Fatal("expected generation left out of the digest")
	}
	if has, err := db.HasMutationReceipt(ctx, "human", "s", "r"); err != nil || has {
		t.Fatalf("receipt before commit %v %v", has, err)
	}
	err = db.executeWrite(ctx, func(conn SQLiteConn) error {
		if replay, err := ReplayMutationReceipt(ctx, conn, key, digest); err != nil || replay != nil {
			t.Fatalf("unseen replay %v %v", replay, err)
		}
		_, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, `{"digest":"`+digest+`","result":{"revision":2}}`)
		return err
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if has, err := db.HasMutationReceipt(ctx, "human", "s", "r"); err != nil || !has {
		t.Fatalf("receipt after commit %v %v", has, err)
	}
	_ = db.readCtx(ctx, false, func(conn SQLiteConn) error {
		replay, err := ReplayMutationReceipt(ctx, conn, key, digest)
		if err != nil || !pyjson.Equal(replay, pyjson.ObjectOf("revision", int64(2))) {
			t.Fatalf("replay %v %v", replay, err)
		}
		if _, err := ReplayMutationReceipt(ctx, conn, key, other); refusalReason(t, err) != "admission_conflict" {
			t.Fatal(err)
		}
		return nil
	})
	if _, err := db.HasMutationReceipt(ctx, "human", "", "r"); err == nil {
		t.Fatal("empty session id accepted")
	}
}
