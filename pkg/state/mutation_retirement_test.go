package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
)

// Ends a created session a hundred days ago
func oldEnded(t *testing.T, db *SessionDB, sid, source string) {
	t.Helper()
	mustCreate(t, db, NewSession{ID: sid, Source: source})
	stamp := now() - 100*86400
	if err := db.writeSQL(context.Background(), "UPDATE sessions SET ended_at=?, started_at=? WHERE id=?", []any{stamp, stamp, sid}, false, nil); err != nil {
		t.Fatal(err)
	}
}

// Admits, claims and settles one input, returning its admission id
func settledAdmission(t *testing.T, db *SessionDB, epoch int64, sid, requestID string) string {
	t.Helper()
	accepted := admitInput(t, db, epoch, "human", sid, requestID, admissionText("x"))
	claim := claimHead(t, db, epoch, sid)
	generation, _ := exactInt(rowValue(claim, "generation"))
	if _, err := db.SettleSessionInput(context.Background(), SettleInput{Epoch: epoch, AdmissionID: objStr(accepted, "admission_id"), Generation: generation, Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	return objStr(accepted, "admission_id")
}

// Admits and claims one input, returning the claim
func startedAdmission(t *testing.T, db *SessionDB, epoch int64, sid, requestID string) *pyjson.Object {
	t.Helper()
	admitInput(t, db, epoch, "human", sid, requestID, admissionText("x"))
	return claimHead(t, db, epoch, sid)
}

// Admission and worker ledger rows naming a session
func ledgerRows(t *testing.T, db *SessionDB, sid string) any {
	t.Helper()
	return scalarOf(t, db.conn, "SELECT (SELECT COUNT(*) FROM session_admissions WHERE target_session_id=?) + (SELECT COUNT(*) FROM worker_executions WHERE session_id=?)", sid, sid)
}

// Delete mutation of sid at its current revision and generation
func deleteMutation(db *SessionDB, epoch int64, sid, requestID string) RuntimeMutation {
	in := mutationAt(db, sid, "delete", requestID, pyjson.NewObject())
	in.Epoch = epoch
	return in
}

// Foreign key check rows of the store
func foreignKeyViolations(t *testing.T, db *SessionDB) []*pyjson.Object {
	t.Helper()
	return rowsOf(t, db, "PRAGMA foreign_key_check")
}

func TestUsedHistoryRetirementIsAtomicAndExact(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	path := t.TempDir() + "/state.db"
	db := mustSessionDB(t, path, false)
	mustCreate(t, db, NewSession{ID: "used", Source: "api_server"})
	if _, err := db.AppendMessage(ctx, "used", map[string]any{"role": "user", "content": "physical history"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	input := SessionInput{Epoch: epoch, PrincipalID: "api", SessionID: "used", RequestID: "once", Payload: admissionText("input")}
	if _, err := db.AdmitSessionInput(ctx, input); err != nil {
		t.Fatal(err)
	}
	row := claimHead(t, db, epoch, "used")
	generation, _ := exactInt(rowValue(row, "generation"))
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "used", Generation: generation, Kind: "compute", AdoptionSecret: "owned-proof"}); err != nil {
		t.Fatal(err)
	}
	workerAppend := pyjson.ObjectOf("messages", []any{pyjson.ObjectOf("role", "assistant", "content", "worker history")})
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "used", Generation: generation, Sequence: 1, Operation: "transcript.append", Payload: workerAppend}
	workerResult := mustReceipt(t, db, scope)
	result := pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "exact reply", "messages", []any{}), "usage", pyjson.ObjectOf("input_tokens", int64(7)))
	retainResult(t, db, epoch, row, result)
	del := deleteMutation(db, epoch, "used", "delete")
	mustExecConn(t, db.conn, "CREATE TRIGGER deny_delete_receipt BEFORE INSERT ON state_meta WHEN NEW.key LIKE 'gateway.mutation.v1.%' BEGIN SELECT RAISE(ABORT,'receipt failed'); END")
	if _, err := db.MutateRuntimeSession(ctx, del); !isIntegrityError(err) || !strings.Contains(err.Error(), "receipt failed") {
		t.Fatalf("denied receipt %v", err)
	}
	if sessionOf(t, db, "used") == nil {
		t.Fatal("refused delete removed the row")
	}
	if admission, _ := db.GetSessionAdmission(ctx, objStr(row, "admission_id")); objStr(admission, "status") != "terminal" {
		t.Fatalf("admission %v", admission)
	}
	mustExecConn(t, db.conn, "DROP TRIGGER deny_delete_receipt")
	receipt := mustMutate(t, db, del)
	db.Close()
	db = mustSessionDB(t, path, false)
	defer db.Close()
	epoch, _ = db.BeginRuntimeEpoch(ctx, "restart")
	if _, err := db.RecoverSessionInputs(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if sessionOf(t, db, "used") != nil || len(contentsOf(t, db, "used", GetMessagesOptions{})) != 0 {
		t.Fatal("history survived")
	}
	if len(foreignKeyViolations(t, db)) != 0 {
		t.Fatal("foreign key violations")
	}
	for _, table := range []string{"session_admissions", "worker_executions", "worker_receipts"} {
		if !pyjson.Equal(scalarOf(t, db.conn, "SELECT COUNT(*) FROM "+table), 0) {
			t.Fatalf("%s rows survived", table)
		}
	}
	del.Epoch = epoch
	if again := mustMutate(t, db, del); !pyjson.Equal(again, receipt) {
		t.Fatalf("delete replay %v", again)
	}
	input.Epoch = epoch
	replay, err := db.AdmitSessionInput(ctx, input)
	if err != nil || objStr(replay, "admission_id") != objStr(row, "admission_id") || objStr(replay, "status") != "terminal" {
		t.Fatalf("replay %v %v", replay, err)
	}
	if !pyjson.Equal(rowValue(replay, "payload"), pyjson.NewObject()) {
		t.Fatal("input history must not be archived in the identity tombstone")
	}
	wantResult := pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "exact reply", "messages", []any{}, "_messages_are_turn_suffix", true), "usage", pyjson.ObjectOf("input_tokens", int64(7)))
	if got := admissionResult(t, db, objStr(row, "admission_id")); !pyjson.Equal(got, wantResult) {
		t.Fatalf("result %v", got)
	}
	digest, err := AdmissionFingerprint("used", map[string]any{"operation": "transcript.append", "payload": workerAppend})
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(rowValue(workerResult, "count"), 1) {
		t.Fatal("worker count")
	}
	if _, err := db.TerminalWorkerReceipt(ctx, "worker", "used", generation, 1, "owned-proof", digest); refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
	if _, err := db.TerminalWorkerReceipt(ctx, "worker", "used", generation, 1, "foreign", digest); refusalReason(t, err) != "permission_denied" {
		t.Fatal(err)
	}
	for _, change := range []func(*SessionInput){
		func(in *SessionInput) { in.Payload = admissionText("changed") },
		func(in *SessionInput) { in.PrincipalID = "foreign" },
		func(in *SessionInput) { in.RequestID = "fresh" },
	} {
		changed := input
		change(&changed)
		if _, err := db.AdmitSessionInput(ctx, changed); err == nil {
			t.Fatal("admission against the deleted session accepted")
		} else {
			refusalReason(t, err)
		}
	}
	late := scope
	late.Epoch, late.Sequence = epoch, 2
	if _, err := db.MutateWorkerExecution(ctx, late); err == nil {
		t.Fatal("late worker write accepted")
	} else {
		refusalReason(t, err)
	}
}

func TestNonterminalObligationsPreventAnyRetirement(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	for _, status := range []string{"queued", "unknown", "started", "worker"} {
		mustCreate(t, db, NewSession{ID: status, Source: "api_server"})
		if status == "worker" {
			if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: status, SessionID: status, Generation: 0, Kind: "compute", AdoptionSecret: "proof"}); err != nil {
				t.Fatal(err)
			}
		} else {
			admitInput(t, db, epoch, "api", status, status, admissionText(status))
			if status != "queued" {
				claimHead(t, db, epoch, status)
			}
			if status == "unknown" {
				epoch, _ = db.BeginRuntimeEpoch(ctx, "next")
				if _, err := db.RecoverSessionInputs(ctx, epoch); err != nil {
					t.Fatal(err)
				}
			}
		}
		reason := "session_busy"
		if status == "unknown" {
			reason = "unknown_execution"
		}
		err := db.executeWrite(ctx, func(conn SQLiteConn) error {
			_, _, err := deleteInTransaction(db, ctx, conn, status, pyjson.NewObject())
			return err
		}, nil)
		if refusalReason(t, err) != reason {
			t.Fatalf("%s -> %v", status, err)
		}
		if sessionOf(t, db, status) == nil {
			t.Fatalf("%s deleted", status)
		}
		if len(rowsOf(t, db, "SELECT 1 FROM state_meta WHERE key LIKE 'gateway.retired_session.v1.%'")) != 0 {
			t.Fatal("retirement marker written")
		}
	}
}

func TestLateAccountingBackfillCannotResurrectARetiredSession(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "retired", Source: "cli"})
	if _, err := db.AppendMessage(ctx, "retired", map[string]any{"role": "user", "content": "history"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	del := deleteMutation(db, epoch, "retired", "delete")
	receipt := mustMutate(t, db, del)
	model, provider := "m", "p"
	calls := 1
	_ = db.RecordAuxiliaryUsage(ctx, "retired", "background_review", AuxiliaryUsage{Model: &model, BillingProvider: &provider, APICallCount: &calls, InputTokens: 3, OutputTokens: 1})
	if err := db.UpdateTokenCounts("retired", TokenCountDelta{InputTokens: 5, OutputTokens: 2, Model: &model}); refusalReason(t, err) != "not_found" {
		t.Fatal(err)
	}
	if err := db.EnsureSession(NewSession{ID: "retired", Source: "unknown"}); refusalReason(t, err) != "not_found" {
		t.Fatal(err)
	}
	if sessionOf(t, db, "retired") != nil {
		t.Fatal("late accounting resurrected a deleted session")
	}
	if !pyjson.Equal(scalarOf(t, db.conn, "SELECT COUNT(*) FROM session_model_usage WHERE session_id='retired'"), 0) {
		t.Fatal("usage rows written")
	}
	if again := mustMutate(t, db, del); !pyjson.Equal(again, receipt) {
		t.Fatalf("replay %v", again)
	}
	if err := db.UpdateTokenCounts("fresh", TokenCountDelta{InputTokens: 1, OutputTokens: 1, Model: &model}); err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(rowValue(sessionOf(t, db, "fresh"), "source"), "unknown") {
		t.Fatal("legacy backfill")
	}
}

// Read receipt payload of a full history read of the session
func historyRead(target string) *pyjson.Object {
	return pyjson.ObjectOf("target", target, "include_ancestors", false, "include_inactive", false, "repair_alternation", false,
		"include_row_ids", false, "include_compacted", false)
}

func TestDeletionTombstoneKeepsOnlyTheClosingWorkerResult(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "gone", Source: "api_server"})
	if _, err := db.AppendMessage(ctx, "gone", map[string]any{"role": "user", "content": "SECRET_HISTORY_LINE"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "gone", Generation: 0, Kind: "compute", AdoptionSecret: "proof"}); err != nil {
		t.Fatal(err)
	}
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "gone", Generation: 0}
	read := scope
	read.Sequence, read.Operation, read.Payload = 1, "compression.history", historyRead("gone")
	history := mustReceipt(t, db, read)
	historyText, _ := pyjson.Dumps(history)
	if !strings.Contains(historyText, "SECRET_HISTORY_LINE") {
		t.Fatal("history read without the line")
	}
	closing := mustReceipt(t, db, finishReceipt(scope, 2))
	if err := db.executeWrite(ctx, func(conn SQLiteConn) error { return retireTerminalReceipts(ctx, conn, []string{"gone"}) }, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(metaBlob(t, db, "SELECT value FROM state_meta WHERE key LIKE 'gateway.terminal_worker.v1.%'"), "SECRET_HISTORY_LINE") {
		t.Fatal("deleted history survives in worker tombstones")
	}
	digest := func(op string, payload *pyjson.Object) string {
		d, err := AdmissionFingerprint("gone", map[string]any{"operation": op, "payload": payload})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	replay, err := db.TerminalWorkerReceipt(ctx, "worker", "gone", 0, 2, "proof", digest("execution.finish", pyjson.NewObject()))
	if err != nil || !pyjson.Equal(replay, closing) {
		t.Fatalf("closing replay %v %v", replay, err)
	}
	if _, err := db.TerminalWorkerReceipt(ctx, "worker", "gone", 0, 1, "proof", digest("execution.finish", pyjson.NewObject())); refusalReason(t, err) != "admission_conflict" {
		t.Fatal(err)
	}
}

func TestDeletingASessionDropsTranscriptCopiesFromItsTerminalResults(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "gone", Source: "api_server"})
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	history := []any{}
	rows := []*pyjson.Object{}
	for turn := 0; turn < 3; turn++ {
		ask := "SECRET_HISTORY q" + pyStr(int64(turn))
		answer := "a" + pyStr(int64(turn))
		admitInput(t, db, epoch, "api", "gone", "r"+pyStr(int64(turn)), admissionText(ask))
		row := claimHead(t, db, epoch, "gone")
		if _, err := db.AppendMessage(ctx, "gone", map[string]any{"role": "user", "content": ask}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
		history = append(history, pyjson.ObjectOf("role", "user", "content", ask), pyjson.ObjectOf("role", "assistant", "content", answer))
		retainResult(t, db, epoch, row, pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", answer, "messages", append([]any{}, history...),
			"last_reasoning", "SECRET_HISTORY why", "completed", true, "api_calls", int64(1), "input_tokens", int64(5)),
			"usage", pyjson.ObjectOf("input_tokens", int64(5), "output_tokens", int64(2))))
		rows = append(rows, row)
		stored := admissionResult(t, db, objStr(row, "admission_id"))
		messages := rowValue(rowValue(stored, "result").(*pyjson.Object), "messages")
		if !pyjson.Equal(messages, []any{pyjson.ObjectOf("role", "assistant", "content", answer)}) {
			t.Fatalf("turn %d receipt %v", turn, stored)
		}
	}
	if ok, err := db.DeleteSession(ctx, "gone", DeleteSessionOptions{}); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	if strings.Contains(metaBlob(t, db, "SELECT value FROM state_meta WHERE key LIKE 'gateway.admission.result.v1.%'"), "SECRET_HISTORY") {
		t.Fatal("deleted history survives in terminal result blobs")
	}
	replay, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "api", SessionID: "gone", RequestID: "r2", Payload: admissionText("SECRET_HISTORY q2")})
	if err != nil || objStr(replay, "admission_id") != objStr(rows[2], "admission_id") || objStr(replay, "status") != "terminal" {
		t.Fatalf("replay %v %v", replay, err)
	}
	want := pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "a2", "messages", []any{}, "completed", true, "api_calls", int64(1), "input_tokens", int64(5),
		"_messages_are_turn_suffix", true), "usage", pyjson.ObjectOf("input_tokens", int64(5), "output_tokens", int64(2)))
	if got := admissionResult(t, db, objStr(replay, "admission_id")); !pyjson.Equal(got, want) {
		t.Fatalf("retired result %v", got)
	}
}

func TestAMalformedUnrelatedRouteOrPolicyRowCannotAbortADelete(t *testing.T) {
	strays := []string{
		"INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES('','other','[]',0)",
		"INSERT INTO gateway_routing(scope,session_key,entry_json,updated_at) VALUES('','other','{bad',0)",
		"INSERT INTO state_meta(key,value) VALUES('gateway.local_policy.v1:other','[]')",
		`INSERT INTO state_meta(key,value) VALUES('gateway.local_policy.v1:other','{"entry": []}')`,
	}
	for _, stray := range strays {
		for _, mode := range []string{"canonical", "legacy", "prune"} {
			installRetirementHooks(t)
			ctx := context.Background()
			db := mustSessionDB(t, t.TempDir()+"/state.db", false)
			epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
			mustCreate(t, db, NewSession{ID: "target", Source: "api_server"})
			mustExecConn(t, db.conn, stray)
			switch mode {
			case "canonical":
				mustMutate(t, db, deleteMutation(db, epoch, "target", "d"))
			case "legacy":
				if ok, err := db.DeleteSession(ctx, "target", DeleteSessionOptions{}); err != nil || !ok {
					t.Fatalf("delete %v %v", ok, err)
				}
			default:
				old := now() - 100*86400
				if err := db.writeSQL(ctx, "UPDATE sessions SET started_at=?, ended_at=? WHERE id=?", []any{old, old, "target"}, false, nil); err != nil {
					t.Fatal(err)
				}
				days := 30.0
				if n, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: &days}); err != nil || n != 1 {
					t.Fatalf("prune %d %v", n, err)
				}
			}
			if sessionOf(t, db, "target") != nil {
				t.Fatalf("%s %s: target survived", mode, stray)
			}
			if retired, err := db.RetiredSession(ctx, "target"); err != nil || !retired {
				t.Fatalf("%s: retired %v %v", mode, retired, err)
			}
			if !pyjson.Equal(scalarOf(t, db.conn, "SELECT (SELECT COUNT(*) FROM gateway_routing) + (SELECT COUNT(*) FROM state_meta WHERE key GLOB 'gateway.local_policy.v1:*')"), 1) {
				t.Fatalf("%s: the unrelated row changed", mode)
			}
			db.Close()
		}
	}
}

func TestDeletedHistoryIsNotRecoverableFromWorkerOrMutationReceipts(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "gone", Source: "cli"})
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	appendText := func(sid, role, content string) int64 {
		id, err := db.AppendMessage(ctx, sid, map[string]any{"role": role, "content": content}, AppendGuards{})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	appendText("gone", "user", "first")
	appendText("gone", "assistant", "a1")
	rewound := appendText("gone", "user", "SECRET_REWOUND")
	appendText("gone", "assistant", "a2")
	rewind := mutationAt(db, "gone", "rewind", "rw", pyjson.ObjectOf("target_message_id", rewound))
	rewind.Epoch = epoch
	receipt := mustMutate(t, db, rewind)
	if text, _ := pyjson.Dumps(receipt); !strings.Contains(text, "SECRET_REWOUND") {
		t.Fatal("rewind receipt without the turn")
	}
	appendText("gone", "user", "SECRET_HISTORY_LINE")
	admitInput(t, db, epoch, "api", "gone", "r", admissionText("x"))
	row := claimHead(t, db, epoch, "gone")
	generation, _ := exactInt(rowValue(row, "generation"))
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "gone", Generation: generation, Kind: "compute", AdoptionSecret: "proof"}); err != nil {
		t.Fatal(err)
	}
	mustReceipt(t, db, WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "gone", Generation: generation, Sequence: 1, Operation: "compression.history", Payload: historyRead("gone")})
	if _, err := db.SettleSessionInput(ctx, SettleInput{Epoch: epoch, AdmissionID: objStr(row, "admission_id"), Generation: generation, Outcome: "failed"}); err != nil {
		t.Fatal(err)
	}
	mustMutate(t, db, deleteMutation(db, epoch, "gone", "d"))
	blob := metaBlob(t, db, "SELECT value FROM state_meta")
	if strings.Contains(blob, "SECRET_REWOUND") || strings.Contains(blob, "SECRET_HISTORY_LINE") {
		t.Fatal("deleted history survives in receipts")
	}
	replay := mustMutate(t, db, rewind)
	want := receipt.Copy()
	want.Set("target_message", nil)
	if !pyjson.Equal(replay, want) {
		t.Fatalf("replay %v", replay)
	}
	digest, _ := AdmissionFingerprint("gone", map[string]any{"operation": "compression.history", "payload": historyRead("gone")})
	if _, err := db.TerminalWorkerReceipt(ctx, "worker", "gone", generation, 1, "proof", digest); refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
}

func TestMutationReceiptsNamingADeletedPhysicalTargetDropItsText(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	dir := t.TempDir()
	db := mustSessionDB(t, dir+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	sid := localSessionFixture(t, db, epoch, dir)
	reset := mutationAt(db, sid, "reset", "reset", pyjson.NewObject())
	reset.Epoch = epoch
	child := objStr(mustMutate(t, db, reset), "target_session_id")
	appendText := func(role, content string) int64 {
		id, err := db.AppendMessage(ctx, child, map[string]any{"role": role, "content": content}, AppendGuards{})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	appendText("user", "first")
	appendText("assistant", "a1")
	target := appendText("user", "SECRET_CHILD_TURN")
	appendText("assistant", "a2")
	rewind := mutationAt(db, sid, "rewind", "rw", pyjson.ObjectOf("target_message_id", target))
	rewind.Epoch = epoch
	if text, _ := pyjson.Dumps(mustMutate(t, db, rewind)); !strings.Contains(text, "SECRET_CHILD_TURN") {
		t.Fatal("rewind receipt without the turn")
	}
	rename := mutationAt(db, sid, "rename", "rn", pyjson.ObjectOf("title", "SECRET_TITLE"))
	rename.Epoch = epoch
	mustMutate(t, db, rename)
	if ok, err := db.DeleteSession(ctx, child, DeleteSessionOptions{}); err != nil || !ok || sessionOf(t, db, sid) != nil {
		t.Fatalf("delete %v %v", ok, err)
	}
	blob := metaBlob(t, db, "SELECT value FROM state_meta")
	if strings.Contains(blob, "SECRET_CHILD_TURN") || strings.Contains(blob, "SECRET_TITLE") {
		t.Fatal("deleted text survives in receipts")
	}
	replay := mustMutate(t, db, rewind)
	if rowValue(replay, "target_message") != nil || !pyjson.Equal(rowValue(replay, "rewound_count"), 2) {
		t.Fatalf("rewind replay %v", replay)
	}
	if rowValue(mustMutate(t, db, rename), "title") != nil {
		t.Fatal("rename replay kept the title")
	}
}

func TestRetirementDropsTheTurnUserRowCopy(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "gone", Source: "api_server"})
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	admitInput(t, db, epoch, "api", "gone", "r1", admissionText("secret ask"))
	row := claimHead(t, db, epoch, "gone")
	user := pyjson.ObjectOf("role", "user", "content", "secret ask", "api_content", "memory: secret ask")
	retainResult(t, db, epoch, row, pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "reply", "current_turn_user_idx", int64(0),
		"messages", []any{user, pyjson.ObjectOf("role", "assistant", "content", "reply")})))
	stored := admissionResult(t, db, objStr(row, "admission_id"))
	if !pyjson.Equal(rowValue(rowValue(stored, "result").(*pyjson.Object), "_turn_user_row"), user) {
		t.Fatalf("stored %v", stored)
	}
	mustMutate(t, db, deleteMutation(db, epoch, "gone", "del"))
	retired := rowValue(admissionResult(t, db, objStr(row, "admission_id")), "result").(*pyjson.Object)
	if _, has := retired.Get("_turn_user_row"); has || !pyjson.Equal(rowValue(retired, "final_response"), "reply") {
		t.Fatalf("retired %v", retired)
	}
}

func TestTombstonesPastTheRetryHorizonAreReclaimedByThePrunePass(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	current := now()
	settleAndDelete := func(sid string, at float64) (SessionInput, string) {
		mustCreate(t, db, NewSession{ID: sid, Source: "api_server"})
		input := SessionInput{Epoch: epoch, PrincipalID: "api", SessionID: sid, RequestID: "r", Payload: admissionText("ask " + sid)}
		if _, err := db.AdmitSessionInput(ctx, input); err != nil {
			t.Fatal(err)
		}
		row := claimHead(t, db, epoch, sid)
		retainResult(t, db, epoch, row, pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "reply "+sid, "messages", []any{})))
		restore := SetWallClock(func() float64 { return at })
		mustMutate(t, db, deleteMutation(db, epoch, sid, "del"))
		restore()
		return input, objStr(row, "admission_id")
	}
	old, oldID := settleAndDelete("old", current-8*86400)
	young, youngID := settleAndDelete("young", current-6*86400)
	rowsNaming := func(sid, admissionID string) []string {
		identity, err := terminalIdentityKey("api", sid, "r")
		if err != nil {
			t.Fatal(err)
		}
		found := []string{}
		for _, k := range []string{TerminalAdmissionPrefix + admissionID, TerminalResultPrefix + admissionID, identity, RetiredPrefix + sid} {
			if _, ok, _ := db.GetMeta(ctx, k); ok {
				found = append(found, k)
			}
		}
		return found
	}
	if len(rowsNaming("old", oldID)) != 4 {
		t.Fatalf("old rows %v", rowsNaming("old", oldID))
	}
	days := 90.0
	if _, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: &days}); err != nil {
		t.Fatal(err)
	}
	if len(rowsNaming("young", youngID)) != 4 {
		t.Fatalf("young rows %v", rowsNaming("young", youngID))
	}
	if got := rowsNaming("old", oldID); !pyjson.Equal(anyStrings(got), []any{RetiredPrefix + "old"}) {
		t.Fatalf("tombstones past the horizon were not reclaimed (the deletion fence stays): %v", got)
	}
	if _, err := db.AdmitSessionInput(ctx, old); refusalReason(t, err) != "not_found" {
		t.Fatal(err)
	}
	replay, err := db.AdmitSessionInput(ctx, young)
	if err != nil || objStr(replay, "admission_id") != youngID || objStr(replay, "status") != "terminal" {
		t.Fatalf("young replay %v %v", replay, err)
	}
	if !pyjson.Equal(rowValue(rowValue(admissionResult(t, db, youngID), "result").(*pyjson.Object), "final_response"), "reply young") {
		t.Fatal("young result")
	}
}

// Legacy delete entry points of the direct store by name
var legacyDeletes = map[string]func(*testing.T, *SessionDB, string) bool{
	"single": func(t *testing.T, db *SessionDB, sid string) bool {
		ok, err := db.DeleteSession(context.Background(), sid, DeleteSessionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	},
	"bulk": func(t *testing.T, db *SessionDB, sid string) bool {
		n, err := db.DeleteSessions(context.Background(), []string{sid}, DeleteSessionsOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return n > 0
	},
	"if_empty": func(t *testing.T, db *SessionDB, sid string) bool {
		ok, err := db.DeleteSessionIfEmpty(context.Background(), sid, "")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	},
	"empty_sweep": func(t *testing.T, db *SessionDB, sid string) bool {
		n, err := db.DeleteEmptySessions(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		return n > 0
	},
	"prune": func(t *testing.T, db *SessionDB, sid string) bool {
		days := 30.0
		n, err := db.PruneSessions(context.Background(), PruneSessionsOptions{OlderThanDays: &days})
		if err != nil {
			t.Fatal(err)
		}
		return n > 0
	},
	"ghost_prune": func(t *testing.T, db *SessionDB, sid string) bool {
		n, err := db.PruneEmptyGhostSessions(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		return n > 0
	},
}

// Refusal reason of a legacy delete entry point against a busy session
func legacyDeleteRefusal(t *testing.T, db *SessionDB, name, sid string) string {
	t.Helper()
	ctx := context.Background()
	var err error
	switch name {
	case "single":
		_, err = db.DeleteSession(ctx, sid, DeleteSessionOptions{})
	case "bulk":
		_, err = db.DeleteSessions(ctx, []string{sid}, DeleteSessionsOptions{})
	case "if_empty":
		_, err = db.DeleteSessionIfEmpty(ctx, sid, "")
	}
	return refusalReason(t, err)
}

func TestLegacyDeleteRetiresTerminalLedgerRowsAndRefusesLiveWork(t *testing.T) {
	for _, name := range []string{"single", "bulk", "if_empty"} {
		installRetirementHooks(t)
		ctx := context.Background()
		db := mustSessionDB(t, t.TempDir()+"/state.db", false)
		epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
		oldEnded(t, db, "settled", "cli")
		admissionID := settledAdmission(t, db, epoch, "settled", "done")
		oldEnded(t, db, "busy", "cli")
		startedAdmission(t, db, epoch, "busy", "live")
		if !legacyDeletes[name](t, db, "settled") {
			t.Fatalf("%s refused the settled session", name)
		}
		if sessionOf(t, db, "settled") != nil || !pyjson.Equal(ledgerRows(t, db, "settled"), 0) {
			t.Fatalf("%s left rows", name)
		}
		tombstone, _ := db.GetSessionAdmission(ctx, admissionID)
		if objStr(tombstone, "status") != "terminal" || !pyjson.Equal(rowValue(tombstone, "payload"), pyjson.NewObject()) {
			t.Fatalf("tombstone %v", tombstone)
		}
		if r := legacyDeleteRefusal(t, db, name, "busy"); r != "session_busy" {
			t.Fatalf("%s -> %s", name, r)
		}
		if sessionOf(t, db, "busy") == nil || !pyjson.Equal(ledgerRows(t, db, "busy"), 1) {
			t.Fatal("busy session touched")
		}
		if len(foreignKeyViolations(t, db)) != 0 {
			t.Fatal("foreign key violations")
		}
		db.Close()
	}
}

func TestLegacyDeletePublishesTheFullRetirementFence(t *testing.T) {
	for _, name := range []string{"single", "bulk", "if_empty", "empty_sweep", "prune", "ghost_prune"} {
		installRetirementHooks(t)
		ctx := context.Background()
		db := mustSessionDB(t, t.TempDir()+"/state.db", false)
		epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
		oldEnded(t, db, "s", "tui")
		if err := db.SaveGatewayRoutingEntry(ctx, "route:s", `{"session_id": "s"}`, ""); err != nil {
			t.Fatal(err)
		}
		request := SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "once", Payload: admissionText("x")}
		admissionID := settledAdmission(t, db, epoch, "s", "once")
		if !legacyDeletes[name](t, db, "s") {
			t.Fatalf("%s did not delete", name)
		}
		retired, _ := db.RetiredSession(ctx, "s")
		if sessionOf(t, db, "s") != nil || !retired {
			t.Fatalf("%s: session or fence", name)
		}
		if routes, _ := db.LoadGatewayRoutingEntries(ctx, ""); len(routes) != 0 {
			t.Fatalf("%s: routes %v", name, routes)
		}
		retry, err := db.AdmitSessionInput(ctx, request)
		if err != nil || objStr(retry, "status") != "terminal" || objStr(retry, "admission_id") != admissionID {
			t.Fatalf("%s: retry %v %v", name, retry, err)
		}
		model := "m"
		if err := db.UpdateTokenCounts("s", TokenCountDelta{InputTokens: 1, OutputTokens: 1, Model: &model}); refusalReason(t, err) != "not_found" {
			t.Fatal(err)
		}
		if sessionOf(t, db, "s") != nil {
			t.Fatal("backfill recreated the row")
		}
		db.Close()
	}
}

func TestLegacyDeleteFencesCascadedDelegateChildren(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	mustCreate(t, db, NewSession{ID: "parent", Source: "cli"})
	mustCreate(t, db, NewSession{ID: "child", Source: "cli", ParentSessionID: "parent", ModelConfig: map[string]any{"_delegate_from": "parent"}})
	admissionID := settledAdmission(t, db, epoch, "child", "sub")
	if ok, err := db.DeleteSession(ctx, "parent", DeleteSessionOptions{}); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	retired, _ := db.RetiredSession(ctx, "child")
	if sessionOf(t, db, "child") != nil || !retired {
		t.Fatal("child not retired")
	}
	retry, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "child", RequestID: "sub", Payload: admissionText("x")})
	if err != nil || objStr(retry, "status") != "terminal" || objStr(retry, "admission_id") != admissionID {
		t.Fatalf("retry %v %v", retry, err)
	}
	if err := db.EnsureSession(NewSession{ID: "child", Source: "unknown"}); refusalReason(t, err) != "not_found" {
		t.Fatal(err)
	}
}

func TestSweepsSkipSessionsWithLiveWorkAndRetireTheRest(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	for _, pair := range [][2]string{{"ghost_settled", "tui"}, {"ghost_busy", "tui"}, {"old_settled", "cli"}, {"old_busy", "cli"}, {"old_plain", "cli"}} {
		oldEnded(t, db, pair[0], pair[1])
	}
	settledAdmission(t, db, epoch, "ghost_settled", "done")
	startedAdmission(t, db, epoch, "ghost_busy", "live")
	settledAdmission(t, db, epoch, "old_settled", "done")
	startedAdmission(t, db, epoch, "old_busy", "live")
	if n, err := db.PruneEmptyGhostSessions(ctx, ""); err != nil || n != 1 {
		t.Fatalf("ghost prune %d %v", n, err)
	}
	if sessionOf(t, db, "ghost_settled") != nil || sessionOf(t, db, "ghost_busy") == nil {
		t.Fatal("a live session must not abort the whole prune")
	}
	if !pyjson.Equal(ledgerRows(t, db, "ghost_settled"), 0) || !pyjson.Equal(ledgerRows(t, db, "ghost_busy"), 1) {
		t.Fatal("ghost ledger rows")
	}
	days := 30.0
	if n, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: &days}); err != nil || n != 2 {
		t.Fatalf("prune %d %v", n, err)
	}
	if sessionOf(t, db, "old_settled") != nil || sessionOf(t, db, "old_plain") != nil || sessionOf(t, db, "old_busy") == nil {
		t.Fatal("prune set")
	}
	if !pyjson.Equal(ledgerRows(t, db, "old_busy"), 1) {
		t.Fatal("old busy ledger")
	}
	if n, err := db.DeleteEmptySessions(ctx, ""); err != nil || n != 0 {
		t.Fatalf("empty sweep %d %v", n, err)
	}
	if sessionOf(t, db, "ghost_busy") == nil || sessionOf(t, db, "old_busy") == nil {
		t.Fatal("empty sweep took a busy session")
	}
	if len(foreignKeyViolations(t, db)) != 0 {
		t.Fatal("foreign key violations")
	}
}

// Local cli owned session plus the reset entry dict of child, the ledger consistency fixture
func localSessionWithReset(t *testing.T, db *SessionDB, epoch int64) (string, *pyjson.Object) {
	t.Helper()
	sid := localSessionFixture(t, db, epoch, t.TempDir())
	reset := localEntryFixture("local:"+sid, "child", localOriginFixture(sid, "human"), time.Now())
	reset.Set("is_fresh_reset", true)
	return sid, reset
}

func TestLocalResetRefusesOverStartedAdmissionThenFencesGeneration(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	sid, reset := localSessionWithReset(t, db, epoch)
	started := startedAdmission(t, db, epoch, sid, "live")
	before := sessionOf(t, db, sid)
	if err := db.ResetLocalTarget(ctx, epoch, sid, reset); refusalReason(t, err) != "session_busy" {
		t.Fatal(err)
	}
	if sessionOf(t, db, "child") != nil || !pyjson.Equal(sessionOf(t, db, sid), before) {
		t.Fatal("refused reset changed rows")
	}
	generation, _ := exactInt(rowValue(started, "generation"))
	if _, err := db.SettleSessionInput(ctx, SettleInput{Epoch: epoch, AdmissionID: objStr(started, "admission_id"), Generation: generation, Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ResetLocalTarget(ctx, epoch, sid, reset); err != nil {
		t.Fatal(err)
	}
	after := sessionOf(t, db, sid)
	beforeGen, _ := exactInt(rowValue(before, "runtime_generation"))
	if !pyjson.Equal(rowValue(after, "end_reason"), "session_reset") || sessionOf(t, db, "child") == nil || !pyjson.Equal(rowValue(after, "runtime_generation"), beforeGen+1) {
		t.Fatalf("after %v", after)
	}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "stale", SessionID: sid, Generation: beforeGen, Kind: "compute", AdoptionSecret: "private-fixture"}); refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
}

func TestLocalResetRefusesOverLiveComputeWorkerButNotAQueuedFollower(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		ctx := context.Background()
		db := mustSessionDB(t, t.TempDir()+"/state.db", false)
		epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
		sid, reset := localSessionWithReset(t, db, epoch)
		scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: sid, Generation: 0}
		if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: sid, Generation: 0, Kind: "compute", AdoptionSecret: "private", RequireIdle: true}); err != nil {
			t.Fatal(err)
		}
		if adopted {
			if _, err := db.AdoptWorkerExecution(ctx, epoch, "worker", sid, 0, "private"); err != nil {
				t.Fatal(err)
			}
		}
		follower := admitInput(t, db, epoch, "human", sid, "follower", admissionText("next"))
		before := sessionOf(t, db, sid)
		if err := db.ResetLocalTarget(ctx, epoch, sid, reset); refusalReason(t, err) != "session_busy" {
			t.Fatal(err)
		}
		if !pyjson.Equal(sessionOf(t, db, sid), before) || sessionOf(t, db, "child") != nil {
			t.Fatal("refused reset changed rows")
		}
		mustReceipt(t, db, appendReceipt(scope, 1, "ok"))
		mustReceipt(t, db, finishReceipt(scope, 2))
		if err := db.ResetLocalTarget(ctx, epoch, sid, reset); err != nil {
			t.Fatal(err)
		}
		beforeGen, _ := exactInt(rowValue(before, "runtime_generation"))
		if !pyjson.Equal(rowValue(sessionOf(t, db, sid), "runtime_generation"), beforeGen+1) {
			t.Fatal("generation")
		}
		if claimed := claimHead(t, db, epoch, sid); objStr(claimed, "admission_id") != objStr(follower, "admission_id") {
			t.Fatalf("claimed %v", claimed)
		}
		db.Close()
	}
}

func TestDiscardingAnUnadmittedRowLeavesNoFenceButAnAdmittedRowIsFenced(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "fresh", Source: "desktop"})
	if ok, err := db.DiscardUnadmittedSession(ctx, "fresh"); err != nil || !ok {
		t.Fatalf("discard %v %v", ok, err)
	}
	if sessionOf(t, db, "fresh") != nil {
		t.Fatal("row kept")
	}
	if _, ok, _ := db.GetMeta(ctx, RetiredPrefix+"fresh"); ok {
		t.Fatal("fence written for an unadmitted row")
	}
	mustCreate(t, db, NewSession{ID: "fresh", Source: "desktop"})
	if sessionOf(t, db, "fresh") == nil {
		t.Fatal("lazy recreation refused")
	}
	mustCreate(t, db, NewSession{ID: "used", Source: "desktop"})
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	settledAdmission(t, db, epoch, "used", "done")
	if ok, err := db.DiscardUnadmittedSession(ctx, "used"); err != nil || !ok {
		t.Fatalf("discard %v %v", ok, err)
	}
	if _, ok, _ := db.GetMeta(ctx, RetiredPrefix+"used"); !ok {
		t.Fatal("admitted row left no fence")
	}
}

func TestLocalResetStampsEndedAtOnTheSameFloatClockAsStartedAt(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	sid, reset := localSessionWithReset(t, db, epoch)
	before := now()
	if err := db.ResetLocalTarget(ctx, epoch, sid, reset); err != nil {
		t.Fatal(err)
	}
	parent := sessionOf(t, db, sid)
	ended, _ := pyFloatOf(rowValue(parent, "ended_at"))
	started, _ := pyFloatOf(rowValue(parent, "started_at"))
	if ended < started || ended < before {
		t.Fatalf("ended %v started %v before %v", ended, started, before)
	}
}

func TestPendingAndIdleProbesDoNotScanFinishedHistory(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "s", Source: "api_server"})
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	if err := db.executeWrite(ctx, func(conn SQLiteConn) error {
		for i := 0; i < 20000; i++ {
			if _, err := conn.ExecContext(ctx, "INSERT INTO session_admissions(admission_id,request_id,principal_id,target_session_id,lineage_json,"+
				"payload_json,payload_digest,intent,status,outcome,owner_epoch,generation) "+
				"VALUES(?,?,'p','s','[]','{}','d','queue','terminal','completed',1,1)", "a"+pyStr(int64(i)), "r"+pyStr(int64(i))); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO worker_executions(execution_id,session_id,kind,owner_epoch,generation,"+
				"status,adoption_digest) VALUES(?,'s','compute',1,1,'terminal','d')", "w"+pyStr(int64(i))); err != nil {
				return err
			}
		}
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	var inner SQLiteConn = db.conn
	for {
		tracked, ok := inner.(*TrackedConn)
		if !ok {
			break
		}
		inner = tracked.SQLiteConn
	}
	raw, ok := inner.(pysqlite.RawConn)
	if !ok {
		t.Fatalf("writer connection %T exposes no raw handle", inner)
	}
	ticks := 0
	if err := pysqlite.SetProgressHandler(raw, func() int { ticks++; return 0 }, 1000); err != nil {
		t.Fatal(err)
	}
	defer pysqlite.SetProgressHandler(raw, nil, 0)
	pinned := withReadConn(ctx, db.conn)
	probes := []struct {
		name string
		run  func()
	}{
		{"pending", func() {
			if _, err := db.ListSessionAdmissions(pinned, "s", true); err != nil {
				t.Fatal(err)
			}
		}},
		{"require_idle", func() {
			if err := db.executeWrite(ctx, func(conn SQLiteConn) error { return db.RequireIdle(ctx, conn, []string{"s"}) }, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"prune_live_check", func() {
			if err := db.executeWrite(ctx, func(conn SQLiteConn) error { _, err := connRow(ctx, conn, liveLedgerSQL, "s", "s"); return err }, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"end_idle", func() {
			if _, err := db.EndIdleLocalSession(ctx, epoch, "s", "s", "idle"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, probe := range probes {
		ticks = 0
		probe.run()
		if ticks >= 20 {
			t.Fatalf("%s scanned finished history (%dk VM steps)", probe.name, ticks)
		}
	}
}

func TestADeletedCompressedRootLeavesItsBranchAndNoReusableID(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	mustCreate(t, db, NewSession{ID: "root", Source: "cli"})
	admitInput(t, db, epoch, "owner", "root", "r1", admissionText("turn"))
	row := claimHead(t, db, epoch, "root")
	retainResult(t, db, epoch, row, pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "reply"), "usage", pyjson.NewObject()))
	if _, err := db.AppendMessage(ctx, "root", map[string]any{"role": "user", "content": "turn"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if !db.TryAcquireCompressionLock("root", "holder", 300) {
		t.Fatal("lock")
	}
	publishChild(t, db, "root", "tip", "cli", "holder", nil, textMessages("user", "summary"))
	db.ReleaseCompressionLock("root", "holder")
	mustCreate(t, db, NewSession{ID: "branch", Source: "cli", ParentSessionID: "tip", ModelConfig: map[string]any{"_branched_from": "tip"}})
	if _, err := db.AppendMessage(ctx, "branch", map[string]any{"role": "user", "content": "copied turn"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	at := now() - 8*86400
	del := deleteMutation(db, epoch, "root", "del")
	restore := SetWallClock(func() float64 { return at })
	receipt := mustMutate(t, db, del)
	restore()
	if !deletedIDsAre(idSet(rowValue(receipt, "deleted_ids")), "root", "tip") {
		t.Fatalf("deleted %v", receipt)
	}
	days := 90.0
	if _, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: &days}); err != nil {
		t.Fatal(err)
	}
	branch := sessionOf(t, db, "branch")
	if rowValue(branch, "parent_session_id") != nil || !strings.Contains(pyStr(rowValue(branch, "model_config")), `"_branched_from": "tip"`) {
		t.Fatalf("branch %v", branch)
	}
	limit := 50
	listed, err := db.ListSessionsRich(ctx, ListSessionsRichOptions{Limit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range listed {
		if objStr(s, "id") == "branch" {
			found = true
		}
	}
	if !found {
		t.Fatal("branch not listed")
	}
	for _, prefix := range []string{TerminalAdmissionPrefix, TerminalIdentityPrefix, TerminalResultPrefix} {
		if keys := metaKeys(t, db, prefix); len(keys) != 0 {
			t.Fatalf("%s keys survived: %v", prefix, keys)
		}
	}
	if got := metaKeys(t, db, RetiredPrefix); !pyjson.Equal(anyStrings(got), []any{RetiredPrefix + "root", RetiredPrefix + "tip"}) {
		t.Fatalf("retired keys %v", got)
	}
	for _, sid := range []string{"root", "tip"} {
		if err := db.EnsureSession(NewSession{ID: sid, Source: "unknown"}); refusalReason(t, err) != "not_found" {
			t.Fatal(err)
		}
		if sessionOf(t, db, sid) != nil {
			t.Fatalf("%s recreated", sid)
		}
	}
}

func TestSweepsKeepTheOwnerARetainedResetChildNeeds(t *testing.T) {
	for _, producer := range []string{"canonical", "session_store"} {
		for _, sweep := range []string{"retention", "empty"} {
			installRetirementHooks(t)
			ctx := context.Background()
			dir := t.TempDir()
			db := mustSessionDB(t, dir+"/state.db", false)
			epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
			sid := localSessionFixture(t, db, epoch, dir)
			var child string
			if producer == "canonical" {
				reset := mutationAt(db, sid, "reset", "reset-0", pyjson.NewObject())
				reset.Epoch = epoch
				child = objStr(mustMutate(t, db, reset), "target_session_id")
			} else {
				receipt, err := db.LocalReceipt(ctx, sid)
				if err != nil {
					t.Fatal(err)
				}
				old := rowValue(receipt, "entry").(*pyjson.Object)
				child = NewUID()
				entry := old.Copy()
				entry.Set("session_id", child)
				entry.Set("is_fresh_reset", true)
				if err := db.ResetLocalTarget(ctx, epoch, sid, entry); err != nil {
					t.Fatal(err)
				}
			}
			for _, pair := range [][2]string{{"user", "hi"}, {"assistant", "hello"}} {
				if _, err := db.AppendMessage(ctx, child, map[string]any{"role": pair[0], "content": pair[1]}, AppendGuards{}); err != nil {
					t.Fatal(err)
				}
			}
			aged := now() - 200*86400
			if err := db.writeSQL(ctx, "UPDATE sessions SET started_at=?, last_activity_at=? WHERE id=?", []any{aged, aged, sid}, false, nil); err != nil {
				t.Fatal(err)
			}
			mustCreate(t, db, NewSession{ID: "stray", Source: "cli"})
			if err := db.writeSQL(ctx, "UPDATE sessions SET started_at=?, last_activity_at=?, ended_at=? WHERE id=?", []any{aged, aged, aged, "stray"}, false, nil); err != nil {
				t.Fatal(err)
			}
			var swept int
			var err error
			if sweep == "retention" {
				days := 90.0
				swept, err = db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: &days})
			} else {
				swept, err = db.DeleteEmptySessions(ctx, "")
			}
			if err != nil || swept != 1 || sessionOf(t, db, "stray") != nil {
				t.Fatalf("%s %s: swept %d %v", producer, sweep, swept, err)
			}
			if sessionOf(t, db, sid) == nil {
				t.Fatal("owner swept")
			}
			if got := contentsOf(t, db, child, GetMessagesOptions{}); !pyjson.Equal(got, []any{"hi", "hello"}) {
				t.Fatalf("child messages %v", got)
			}
			var target string
			if err := db.readCtx(ctx, false, func(conn SQLiteConn) error {
				var err error
				target, err = localPhysicalTarget(ctx, conn, sid)
				return err
			}); err != nil || target != child {
				t.Fatalf("physical target %q %v", target, err)
			}
			admitted := admitInput(t, db, epoch, "human", sid, "after", admissionText("after"))
			if objStr(admitted, "status") != "queued" {
				t.Fatalf("admitted %v", admitted)
			}
			db.Close()
		}
	}
}

// Installs a lifecycle hook invoker recording on_session_delete events with the row state after commit
func recordDeleteEvents(t *testing.T, db *SessionDB, before func() error) *[]*pyjson.Object {
	t.Helper()
	events := &[]*pyjson.Object{}
	SetLifecycleHookInvoker(func(ctx context.Context, hookName string, kwargs map[string]any) ([]any, error) {
		if hookName != "on_session_delete" {
			return nil, nil
		}
		var firstErr error
		if before != nil {
			firstErr = before()
		}
		event := pyjson.NewObject()
		for k, v := range kwargs {
			event.Set(k, v)
		}
		row, _ := db.GetSession(ctx, pyStr(kwargs["session_id"]))
		if row == nil {
			event.Set("row_after", nil)
		} else {
			event.Set("row_after", pyTruthy(rowValue(row, "archived")))
		}
		*events = append(*events, event)
		return nil, firstErr
	})
	t.Cleanup(func() { SetLifecycleHookInvoker(nil) })
	return events
}

// Canonical mutation of sid at its current revision
func canonicalMutate(t *testing.T, db *SessionDB, sid, operation string, payload *pyjson.Object) *pyjson.Object {
	t.Helper()
	epoch, err := db.BeginRuntimeEpoch(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	in := mutationAt(db, sid, operation, operation+"-"+sid, payload)
	in.Epoch = epoch
	return mustMutate(t, db, in)
}

func TestEveryDeleteAndArchivePathFiresOnceAfterCommit(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	events := recordDeleteEvents(t, db, nil)
	for _, sid := range []string{"cli", "bulk1", "bulk2", "empty", "old", "desk", "arch", "desk-arch"} {
		mustCreate(t, db, NewSession{ID: sid, Source: "cli"})
		if sid != "empty" {
			if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": "user", "content": "hi"}, AppendGuards{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, sid := range []string{"empty", "old"} {
		if err := db.EndSession(sid, "done"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tip := range []string{"arch", "desk-arch"} {
		root := tip + "-root"
		mustCreate(t, db, NewSession{ID: root, Source: "cli"})
		if _, err := db.AppendMessage(ctx, root, map[string]any{"role": "user", "content": "hi"}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
		if err := db.EndSession(root, "compression"); err != nil {
			t.Fatal(err)
		}
		if err := db.writeSQL(ctx, "UPDATE sessions SET parent_session_id = ? WHERE id = ?", []any{root, tip}, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := db.DeleteSession(ctx, "cli", DeleteSessionOptions{}); err != nil || !ok {
		t.Fatalf("delete cli %v %v", ok, err)
	}
	if ok, err := db.DeleteSession(ctx, "missing", DeleteSessionOptions{}); err != nil || ok {
		t.Fatalf("delete missing %v %v", ok, err)
	}
	if n, err := db.DeleteSessions(ctx, []string{"bulk1", "bulk2", "nope"}, DeleteSessionsOptions{}); err != nil || n != 2 {
		t.Fatalf("bulk delete %d %v", n, err)
	}
	if n, err := db.DeleteEmptySessions(ctx, ""); err != nil || n != 1 {
		t.Fatalf("empty sweep %d %v", n, err)
	}
	days := 0.0
	if n, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: &days}); err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
	canonicalMutate(t, db, "desk", "delete", pyjson.NewObject())
	if ok, err := db.SetSessionArchived(ctx, "arch", true, true); err != nil || !ok {
		t.Fatalf("archive %v %v", ok, err)
	}
	if _, err := db.SetSessionArchived(ctx, "arch", true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionArchived(ctx, "arch", false, true); err != nil {
		t.Fatal(err)
	}
	canonicalMutate(t, db, "desk-arch", "sidebar", pyjson.ObjectOf("archived", true))
	if _, err := db.ArchiveStaleSessions(ctx, 0, true); err != nil {
		t.Fatal(err)
	}
	got := []any{}
	for _, e := range *events {
		deleted, _ := e.Get("deleted_ids")
		got = append(got, []any{rowValue(e, "session_id"), rowValue(e, "reason"), deleted, rowValue(e, "row_after")})
	}
	want := []any{
		[]any{"cli", "delete", nil, nil},
		[]any{"bulk1", "delete", []any{"bulk1", "bulk2"}, nil},
		[]any{"empty", "empty_sweep", nil, nil},
		[]any{"old", "prune", nil, nil},
		[]any{"desk", "delete", nil, nil},
		[]any{"arch", "archive", []any{"arch", "arch-root"}, true},
		[]any{"desk-arch", "archive", []any{"desk-arch", "desk-arch-root"}, true},
	}
	if !pyjson.Equal(jsonRoundTrip(got), jsonRoundTrip(want)) {
		t.Fatalf("events %v", got)
	}
	for _, e := range *events {
		if !pyjson.Equal(rowValue(e, "profile"), rowValue((*events)[0], "profile")) {
			t.Fatal("profiles differ")
		}
	}
}

func TestRaisingCallbackNeverBreaksDeletion(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	events := recordDeleteEvents(t, db, func() error { return &stateTypedError{"RuntimeError", "plugin down"} })
	mustCreate(t, db, NewSession{ID: "s1", Source: "cli"})
	mustCreate(t, db, NewSession{ID: "s2", Source: "cli"})
	if ok, err := db.DeleteSession(ctx, "s1", DeleteSessionOptions{}); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	if ok, err := db.SetSessionArchived(ctx, "s2", true, true); err != nil || !ok {
		t.Fatalf("archive %v %v", ok, err)
	}
	if sessionOf(t, db, "s1") != nil || !pyTruthy(rowValue(sessionOf(t, db, "s2"), "archived")) {
		t.Fatal("state")
	}
	ids := []any{}
	for _, e := range *events {
		ids = append(ids, rowValue(e, "session_id"))
	}
	if !pyjson.Equal(ids, []any{"s1", "s2"}) {
		t.Fatalf("events %v", ids)
	}
}

// Admits, claims and settles one input of a fresh session holding one message
func settledSession(t *testing.T, db *SessionDB, epoch int64, sid string) {
	t.Helper()
	mustCreate(t, db, NewSession{ID: sid, Source: "cli"})
	if _, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": "user", "content": "kept"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	settledAdmission(t, db, epoch, sid, "r")
}

// The statements an older release's delete_session runs, in its one write transaction
func olderReleaseDelete(path, sid string) error {
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	run := func() error {
		for _, stmt := range []string{"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id = ?",
			"DELETE FROM messages WHERE session_id = ?", "DELETE FROM sessions WHERE id = ?"} {
			if _, err := conn.ExecContext(ctx, stmt, sid); err != nil {
				return err
			}
		}
		_, err := conn.ExecContext(ctx, "COMMIT")
		return err
	}
	if err := run(); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	return nil
}

func TestAnOlderReleaseDeleteOfALedgeredSessionIsRefusedWithTheWayBack(t *testing.T) {
	t.Setenv("ICEBATH_CJK_FTS", "off")
	installRetirementHooks(t)
	ctx := context.Background()
	path := t.TempDir() + "/state.db"
	db := mustSessionDB(t, path, false)
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	settledSession(t, db, epoch, "ledgered")
	mustCreate(t, db, NewSession{ID: "plain", Source: "cli"})
	db.Close()
	err := olderReleaseDelete(path, "ledgered")
	if !isIntegrityError(err) {
		t.Fatalf("want an integrity refusal, got %v", err)
	}
	message := err.Error()
	for _, fragment := range []string{"newer Hermes", "Downgrading is unsupported", "hermes update", "pre-update snapshot"} {
		if !strings.Contains(message, fragment) {
			t.Fatalf("message lacks %q: %s", fragment, message)
		}
	}
	if ClassifyPersistenceError(err) != "unknown" {
		t.Fatalf("classified %q", ClassifyPersistenceError(err))
	}
	if err := olderReleaseDelete(path, "plain"); err != nil {
		t.Fatal(err)
	}
	db = mustSessionDB(t, path, false)
	defer db.Close()
	if got := contentsOf(t, db, "ledgered", GetMessagesOptions{}); !pyjson.Equal(got, []any{"kept"}) {
		t.Fatalf("messages %v", got)
	}
	if sessionOf(t, db, "plain") != nil {
		t.Fatal("plain survived")
	}
	if ok, err := db.DeleteSession(ctx, "ledgered", DeleteSessionOptions{}); err != nil || !ok || sessionOf(t, db, "ledgered") != nil {
		t.Fatalf("own delete %v %v", ok, err)
	}
}

func TestAnUpgradedStoreGainsTheGuardOnItsNextOpen(t *testing.T) {
	t.Setenv("ICEBATH_CJK_FTS", "off")
	ctx := context.Background()
	path := t.TempDir() + "/state.db"
	db := mustSessionDB(t, path, false)
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	settledSession(t, db, epoch, "s")
	db.Close()
	rawExec(t, path, "DROP TRIGGER sessions_runtime_ledger_delete_guard")
	mustSessionDB(t, path, false).Close()
	err := olderReleaseDelete(path, "s")
	if !isIntegrityError(err) || !strings.Contains(err.Error(), "newer Hermes") {
		t.Fatalf("guard missing after reopen: %v", err)
	}
}

// Exports a session, deletes it through the canonical mutation and prunes its tombstones at once
func exportedThenDeleted(t *testing.T, db *SessionDB, epoch int64, sid string) *pyjson.Object {
	t.Helper()
	ctx := context.Background()
	mustCreate(t, db, NewSession{ID: sid, Source: "cli"})
	if _, err := db.AppendMessage(ctx, sid, map[string]any{"role": "user", "content": "keep me"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	exported, err := db.ExportSession(ctx, sid, false, false)
	if err != nil {
		t.Fatal(err)
	}
	mustMutate(t, db, deleteMutation(db, epoch, sid, "del-"+sid))
	if err := db.executeWrite(ctx, func(conn SQLiteConn) error { return pruneRetiredTombstones(ctx, conn, 0) }, nil); err != nil {
		t.Fatal(err)
	}
	return exported
}

func TestImportSessionsRefusesADeletedIDByName(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	exported := exportedThenDeleted(t, db, epoch, "s")
	result, err := db.ImportSessions(ctx, []any{exported})
	if err != nil || rowValue(result, "ok") != false || !pyjson.Equal(rowValue(result, "imported"), 0) {
		t.Fatalf("result %v %v", result, err)
	}
	found := false
	for _, e := range rowValue(result, "errors").([]any) {
		text := pyStr(e)
		if strings.Contains(text, "s") && strings.Contains(text, "deleted") {
			found = true
		}
	}
	if !found || sessionOf(t, db, "s") != nil {
		t.Fatalf("the import put the deleted row back beside its fence: %v", result)
	}
}

func TestTheRuntimeImportRefusesADeletedIDByName(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	exported := exportedThenDeleted(t, db, epoch, "s")
	_, err := db.MutateRuntimeSession(ctx, RuntimeMutation{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "restore", ExpectedRevision: 0,
		Operation: "import", Payload: pyjson.ObjectOf("sessions", []any{exported})})
	if refusalReason(t, err) != "admission_conflict" {
		t.Fatal(err)
	}
	refusal := err.(*RuntimeStoreError)
	if refusal.Field == nil || *refusal.Field != "sessions" || refusal.Detail == nil || !strings.Contains(*refusal.Detail, "deleted") {
		t.Fatalf("refusal %v %v", refusal.Field, refusal.Detail)
	}
	if sessionOf(t, db, "s") != nil {
		t.Fatal("row restored")
	}
}

func TestASweptSessionRestoresOnceItsFenceAgesOut(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	mustCreate(t, db, NewSession{ID: "swept", Source: "cron"})
	if _, err := db.AppendMessage(ctx, "swept", map[string]any{"role": "user", "content": "keep me"}, AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	exported, err := db.ExportSession(ctx, "swept", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("swept", "test"); err != nil {
		t.Fatal(err)
	}
	if n, err := db.PruneSessions(ctx, PruneSessionsOptions{}); err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
	if err := db.executeWrite(ctx, func(conn SQLiteConn) error { return pruneRetiredTombstones(ctx, conn, 0) }, nil); err != nil {
		t.Fatal(err)
	}
	result, err := db.ImportSessions(ctx, []any{exported})
	if err != nil || !pyjson.Equal(rowValue(result, "imported"), 1) || sessionOf(t, db, "swept") == nil {
		t.Fatalf("import %v %v", result, err)
	}
}

// Deletes sid through the canonical mutation with the clock eight days back
func deleteEightDaysAgo(t *testing.T, db *SessionDB, epoch int64, sid string) {
	t.Helper()
	at := now() - 8*86400
	del := deleteMutation(db, epoch, sid, "del")
	restore := SetWallClock(func() float64 { return at })
	defer restore()
	mustMutate(t, db, del)
}

func TestADeletedSessionIsNeverRecreatedAfterTheHorizon(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	mustCreate(t, db, NewSession{ID: "s", Source: "api_server"})
	input := SessionInput{Epoch: epoch, PrincipalID: "api", SessionID: "s", RequestID: "r1", Payload: admissionText("charge card")}
	if _, err := db.AdmitSessionInput(ctx, input); err != nil {
		t.Fatal(err)
	}
	row := claimHead(t, db, epoch, "s")
	retainResult(t, db, epoch, row, pyjson.ObjectOf("result", pyjson.ObjectOf("final_response", "done"), "usage", pyjson.NewObject()))
	deleteEightDaysAgo(t, db, epoch, "s")
	days := 90.0
	if _, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: &days}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.GetMeta(ctx, RetiredPrefix+"s"); !ok {
		t.Fatal("fence reclaimed")
	}
	if err := db.EnsureSession(NewSession{ID: "s", Source: "unknown"}); refusalReason(t, err) != "not_found" {
		t.Fatal(err)
	}
	if _, err := db.AdmitSessionInput(ctx, input); refusalReason(t, err) != "not_found" {
		t.Fatal(err)
	}
	if sessionOf(t, db, "s") != nil {
		t.Fatal("recreated")
	}
}

func TestASweptSessionFenceAgesOutAtTheHorizon(t *testing.T) {
	installRetirementHooks(t)
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	mustCreate(t, db, NewSession{ID: "swept", Source: "cron"})
	if err := db.EndSession("swept", "test"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, db, NewSession{ID: "empty", Source: "cli"})
	mustCreate(t, db, NewSession{ID: "deleted", Source: "cli"})
	deleteEightDaysAgo(t, db, epoch, "deleted")
	if n, err := db.PruneSessions(ctx, PruneSessionsOptions{}); err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
	if ok, err := db.DeleteSessionIfEmpty(ctx, "empty", ""); err != nil || !ok {
		t.Fatalf("delete if empty %v %v", ok, err)
	}
	for _, sid := range []string{"swept", "empty"} {
		if _, ok, _ := db.GetMeta(ctx, RetiredPrefix+sid); !ok {
			t.Fatalf("%s unfenced within the horizon", sid)
		}
	}
	if err := db.executeWrite(ctx, func(conn SQLiteConn) error { return pruneRetiredTombstones(ctx, conn, 0) }, nil); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"swept", "empty"} {
		if _, ok, _ := db.GetMeta(ctx, RetiredPrefix+sid); ok {
			t.Fatalf("%s: a sweep's marker never ages out", sid)
		}
	}
	if _, ok, _ := db.GetMeta(ctx, RetiredPrefix+"deleted"); !ok {
		t.Fatal("user delete marker aged out")
	}
}

func TestASweptAPISessionKeepsItsFenceOnlyWhenADoorDerivedItsID(t *testing.T) {
	for _, c := range []struct {
		sid     string
		agesOut bool
	}{{"user-42", true}, {"api-0123456789abcdef", false}} {
		installRetirementHooks(t)
		ctx := context.Background()
		db := mustSessionDB(t, t.TempDir()+"/state.db", false)
		mustCreate(t, db, NewSession{ID: c.sid, Source: "api_server"})
		if err := db.EndSession(c.sid, "test"); err != nil {
			t.Fatal(err)
		}
		if n, err := db.PruneSessions(ctx, PruneSessionsOptions{}); err != nil || n != 1 {
			t.Fatalf("prune %d %v", n, err)
		}
		if err := db.executeWrite(ctx, func(conn SQLiteConn) error { return pruneRetiredTombstones(ctx, conn, 0) }, nil); err != nil {
			t.Fatal(err)
		}
		_, fenced, _ := db.GetMeta(ctx, RetiredPrefix+c.sid)
		if !fenced != c.agesOut {
			t.Fatalf("%s fenced %v", c.sid, fenced)
		}
		var code string
		if err := db.readCtx(ctx, false, func(conn SQLiteConn) error {
			var err error
			code, _, err = CreateConflict(ctx, conn, c.sid)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if (code == "") != c.agesOut {
			t.Fatalf("%s conflict %q", c.sid, code)
		}
		db.Close()
	}
}

func TestAPermanentMarkerAnEarlierReleaseWroteForASweptAPISessionStays(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	marker, _ := pyjson.Dumps(pyjson.ObjectOf("retired_at", now()-30*86400))
	if err := db.SetMeta(ctx, RetiredPrefix+"user-42", marker); err != nil {
		t.Fatal(err)
	}
	if err := db.executeWrite(ctx, func(conn SQLiteConn) error { return pruneRetiredTombstones(ctx, conn, now()-retryHorizonS) }, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.GetMeta(ctx, RetiredPrefix+"user-42"); !ok {
		t.Fatal("permanent marker reclaimed")
	}
	if err := db.readCtx(ctx, false, func(conn SQLiteConn) error {
		code, message, err := CreateConflict(ctx, conn, "user-42")
		if err != nil {
			return err
		}
		if code != "session_deleted" || message != "Session was deleted; its id cannot be reused" {
			t.Fatalf("conflict %q %q", code, message)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
