package state

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Store with session s, a boot epoch and a registered compute worker at generation zero
func setupWorker(t *testing.T) (*SessionDB, string, int64, WorkerReceipt) {
	t.Helper()
	ctx := context.Background()
	path := t.TempDir() + "/state.db"
	db := mustSessionDB(t, path, false)
	if err := db.CreateSession(NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "boot")
	if err != nil {
		t.Fatal(err)
	}
	assignment := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "s", Generation: 0}
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "s", Generation: 0, Kind: "compute", AdoptionSecret: "private-fixture"}); err != nil {
		t.Fatal(err)
	}
	return db, path, epoch, assignment
}

// The worker's production text write, one transcript.append receipt
func appendReceipt(scope WorkerReceipt, sequence int64, content string) WorkerReceipt {
	scope.Sequence, scope.Operation = sequence, "transcript.append"
	scope.Payload = pyjson.ObjectOf("messages", []any{pyjson.ObjectOf("role", "assistant", "content", content)})
	return scope
}

func finishReceipt(scope WorkerReceipt, sequence int64) WorkerReceipt {
	scope.Sequence, scope.Operation, scope.Payload = sequence, "execution.finish", pyjson.NewObject()
	return scope
}

// Result of a receipt that must succeed
func mustReceipt(t *testing.T, db *SessionDB, in WorkerReceipt) *pyjson.Object {
	t.Helper()
	out, err := db.MutateWorkerExecution(context.Background(), in)
	if err != nil {
		t.Fatalf("%s %v", in.Operation, err)
	}
	return out
}

// Refusal reason of a receipt that must fail
func receiptRefusal(t *testing.T, db *SessionDB, in WorkerReceipt) string {
	t.Helper()
	_, err := db.MutateWorkerExecution(context.Background(), in)
	return refusalReason(t, err)
}

func totalChangesOf(t *testing.T, db *SessionDB) int64 {
	t.Helper()
	n, err := sqliteTotalChanges(db.conn)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWorkerReceiptsRequireExactSequenceAssignmentAndAdoption(t *testing.T) {
	ctx := context.Background()
	db, path, epoch, assignment := setupWorker(t)
	defer func() { db.Close() }()
	if claim, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "s"}); err != nil || claim != nil {
		t.Fatalf("claim %v %v", claim, err)
	}
	args := appendReceipt(assignment, 1, "kept output")
	result := mustReceipt(t, db, args)
	if again := mustReceipt(t, db, args); !pyjson.Equal(again, result) {
		t.Fatalf("replay %v != %v", again, result)
	}
	if len(messageContents(t, db, "s")) != 1 {
		t.Fatal("one message expected")
	}
	changed := appendReceipt(assignment, 1, "different")
	if r := receiptRefusal(t, db, changed); r != "admission_conflict" {
		t.Fatal(r)
	}
	skipped := appendReceipt(assignment, 3, "kept output")
	if r := receiptRefusal(t, db, skipped); r != "invalid_params" {
		t.Fatal(r)
	}
	other := args
	other.SessionID = "other"
	if r := receiptRefusal(t, db, other); r != "permission_denied" {
		t.Fatal(r)
	}
	stale := args
	stale.Generation = 1
	if r := receiptRefusal(t, db, stale); r != "stale_generation" {
		t.Fatal(r)
	}
	newEpoch, _ := db.BeginRuntimeEpoch(ctx, "replacement")
	if _, err := db.RecoverSessionInputs(ctx, newEpoch); err != nil {
		t.Fatal(err)
	}
	if r := receiptRefusal(t, db, args); r != "stale_epoch" {
		t.Fatal(r)
	}
	fresh := args
	fresh.Epoch = newEpoch
	if r := receiptRefusal(t, db, fresh); r != "stale_epoch" {
		t.Fatal(r)
	}
	if _, err := db.AdoptWorkerExecution(ctx, newEpoch, "worker", "s", 0, "wrong"); refusalReason(t, err) != "permission_denied" {
		t.Fatal(err)
	}
	adopted, err := db.AdoptWorkerExecution(ctx, newEpoch, "worker", "s", 0, "private-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, has := adopted.Get("adoption_digest"); has || !pyjson.Equal(rowValue(adopted, "owner_epoch"), newEpoch) {
		t.Fatalf("adopted %v", adopted)
	}
	if again := mustReceipt(t, db, fresh); !pyjson.Equal(again, result) {
		t.Fatalf("replay after adoption %v", again)
	}
	next := appendReceipt(assignment, 2, "next")
	next.Epoch = newEpoch
	mustReceipt(t, db, next)
	finishArgs := finishReceipt(assignment, 3)
	finishArgs.Epoch = newEpoch
	finished := mustReceipt(t, db, finishArgs)
	db.Close()
	db = mustSessionDB(t, path, false)
	before := totalChangesOf(t, db)
	if again := mustReceipt(t, db, finishArgs); !pyjson.Equal(again, finished) {
		t.Fatalf("finish replay %v", again)
	}
	if again := mustReceipt(t, db, fresh); !pyjson.Equal(again, result) {
		t.Fatalf("append replay %v", again)
	}
	if totalChangesOf(t, db) != before {
		t.Fatal("replays wrote")
	}
	if !pyjson.Equal(finished, pyjson.ObjectOf("status", "terminal")) {
		t.Fatalf("finished %v", finished)
	}
	changedNew := appendReceipt(assignment, 1, "changed")
	changedNew.Epoch = newEpoch
	if r := receiptRefusal(t, db, changedNew); r != "admission_conflict" {
		t.Fatal(r)
	}
	if _, err := db.AdoptWorkerExecution(ctx, newEpoch, "worker", "s", 0, "private-fixture"); refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
	late := appendReceipt(assignment, 4, "kept output")
	late.Epoch = newEpoch
	if r := receiptRefusal(t, db, late); r != "stale_generation" {
		t.Fatal(r)
	}
	if got := messageContents(t, db, "s"); !pyjson.Equal(anyStrings(got), []any{"kept output", "next"}) {
		t.Fatalf("contents %v", got)
	}
	if _, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: newEpoch, PrincipalID: "human", SessionID: "s", RequestID: "next", Payload: pyjson.NewObject()}); err != nil {
		t.Fatal(err)
	}
	claim, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: newEpoch, SessionID: "s"})
	if err != nil || claim == nil {
		t.Fatalf("claim %v %v", claim, err)
	}
	if generation, _ := exactInt(rowValue(claim, "generation")); generation <= assignment.Generation {
		t.Fatalf("generation %v", claim)
	}
	if r := receiptRefusal(t, db, fresh); r != "stale_generation" {
		t.Fatal(r)
	}
	if r := receiptRefusal(t, db, finishArgs); r != "stale_generation" {
		t.Fatal(r)
	}
}

func TestWorkerMutationAndReceiptRollBackTogether(t *testing.T) {
	for _, content := range []string{"once", "a\xed\xa0\x80b"} {
		ctx := context.Background()
		db, path, _, assignment := setupWorker(t)
		mustExecConn(t, db.conn, "CREATE TRIGGER reject_receipt BEFORE INSERT ON worker_receipts BEGIN SELECT RAISE(ABORT, 'receipt fixture'); END")
		if err := db.CreateSession(NewSession{ID: "direct", Source: "test"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AppendMessage(ctx, "direct", map[string]any{"role": "assistant", "content": content}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
		expected := messageContents(t, db, "direct")[0]
		args := appendReceipt(assignment, 1, content)
		_, err := db.MutateWorkerExecution(ctx, args)
		if !isIntegrityError(err) || !strings.Contains(err.Error(), "receipt fixture") {
			t.Fatalf("want the receipt fixture integrity error, got %v", err)
		}
		if len(messageContents(t, db, "s")) != 0 || !pyjson.Equal(rowValue(mustSession(t, db, "s"), "message_count"), 0) {
			t.Fatal("rolled back append left rows")
		}
		if !pyjson.Equal(scalarOf(t, db.conn, "SELECT last_sequence FROM worker_executions WHERE execution_id='worker'"), 0) {
			t.Fatal("last_sequence moved")
		}
		mustExecConn(t, db.conn, "DROP TRIGGER reject_receipt")
		receipt := mustReceipt(t, db, args)
		db.Close()
		db = mustSessionDB(t, path, false)
		if again := mustReceipt(t, db, args); !pyjson.Equal(again, receipt) {
			t.Fatalf("replay %v", again)
		}
		if got := messageContents(t, db, "s")[0]; got != expected {
			t.Fatalf("content %q != %q", got, expected)
		}
		if content != expected {
			if r := receiptRefusal(t, db, appendReceipt(assignment, 1, expected)); r != "admission_conflict" {
				t.Fatal(r)
			}
		}
		if len(messageContents(t, db, "s")) != 1 || !pyjson.Equal(rowValue(mustSession(t, db, "s"), "message_count"), 1) {
			t.Fatal("one message expected")
		}
		db.Close()
	}
}

func TestWorkerAppendPreservesCompressionWriteGuards(t *testing.T) {
	for _, closed := range []bool{false, true} {
		ctx := context.Background()
		db, path, _, assignment := setupWorker(t)
		args := appendReceipt(assignment, 1, "before closure")
		receipt := mustReceipt(t, db, args)
		if !db.TryAcquireCompressionLock("s", "compressor", 300) {
			t.Fatal("lock")
		}
		if closed {
			if err := db.EndSession("s", "compression"); err != nil {
				t.Fatal(err)
			}
			if err := db.CreateSession(NewSession{ID: "tip", Source: "test", ParentSessionID: "s"}); err != nil {
				t.Fatal(err)
			}
		}
		before := mustSession(t, db, "s")
		messages, _ := db.GetMessages(ctx, "s", GetMessagesOptions{})
		next := appendReceipt(assignment, 2, "new output")
		if closed {
			var closedErr *CompressionSessionClosedError
			if _, err := db.AppendMessage(ctx, "s", map[string]any{"role": "assistant", "content": "ordinary"}, AppendGuards{}); !errors.As(err, &closedErr) {
				t.Fatalf("direct append %v", err)
			}
			if _, err := db.MutateWorkerExecution(ctx, next); !errors.As(err, &closedErr) {
				t.Fatalf("worker append %v", err)
			}
			after, _ := db.GetMessages(ctx, "s", GetMessagesOptions{})
			if !pyjson.Equal(mustSession(t, db, "s"), before) || !pyjson.Equal(anyObjects(after), anyObjects(messages)) {
				t.Fatal("closed session changed")
			}
			if !pyjson.Equal(scalarOf(t, db.conn, "SELECT COUNT(*) FROM worker_receipts"), 1) || !pyjson.Equal(scalarOf(t, db.conn, "SELECT last_sequence FROM worker_executions"), 1) {
				t.Fatal("receipt bookkeeping moved")
			}
		} else {
			if _, err := db.AppendMessage(ctx, "s", map[string]any{"role": "assistant", "content": "ordinary"}, AppendGuards{}); err != nil {
				t.Fatal(err)
			}
			mustReceipt(t, db, next)
			if got := messageContents(t, db, "s"); !pyjson.Equal(anyStrings(got), []any{"before closure", "ordinary", "new output"}) {
				t.Fatalf("contents %v", got)
			}
		}
		db.Close()
		db = mustSessionDB(t, path, false)
		if again := mustReceipt(t, db, args); !pyjson.Equal(again, receipt) {
			t.Fatalf("replay %v", again)
		}
		db.Close()
	}
}

// Row objects as a JSON list value
func anyObjects(rows []*pyjson.Object) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

func TestStructuredWorkerReceiptsAreAtomicAndScoped(t *testing.T) {
	ctx := context.Background()
	db := mustSessionDB(t, t.TempDir()+"/state.db", false)
	defer db.Close()
	for _, n := range []NewSession{{ID: "assigned", Source: "cli"}, {ID: "foreign", Source: "cli"}} {
		if err := db.CreateSession(n); err != nil {
			t.Fatal(err)
		}
	}
	epoch, _ := db.BeginRuntimeEpoch(ctx, "owner")
	if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "assigned", Generation: 0, Kind: "compute", AdoptionSecret: "private"}); err != nil {
		t.Fatal(err)
	}
	scope := WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "assigned", Generation: 0}
	apply := func(seq int64, op string, payload any, override func(*WorkerReceipt)) (*pyjson.Object, error) {
		in := scope
		in.Sequence, in.Operation, in.Payload = seq, op, payload
		if override != nil {
			override(&in)
		}
		return db.MutateWorkerExecution(ctx, in)
	}
	acquired, err := apply(1, "turn.acquire", pyjson.ObjectOf("holder", "worker-lease", "ttl_seconds", int64(300)), nil)
	if err != nil || !pyTruthy(resultValue(acquired, "value")) {
		t.Fatalf("acquire %v %v", acquired, err)
	}
	surrogate := "\xed\xa0\x80"
	messages := []any{
		pyjson.ObjectOf("role", "user", "content", []any{pyjson.ObjectOf("type", "text", "text", "hello"+surrogate)}, "api_content", "wire"),
		pyjson.ObjectOf("role", "assistant", "content", nil, "reasoning", "private reason",
			"tool_calls", []any{pyjson.ObjectOf("id", "call1", "type", "function", "function", pyjson.ObjectOf("name", "terminal", "arguments", "{}"))}),
		pyjson.ObjectOf("role", "tool", "content", "tool-marker", "tool_call_id", "call1", "tool_name", "terminal"),
	}
	payload := pyjson.ObjectOf("messages", messages, "turn_lease_holder", "worker-lease")
	receipt, err := apply(2, "transcript.append", payload, nil)
	if err != nil || !pyjson.Equal(resultValue(receipt, "count"), 3) {
		t.Fatalf("append %v %v", receipt, err)
	}
	for _, m := range resultValue(receipt, "annotations").([]any) {
		if id, ok := exactInt(rowValue(m.(*pyjson.Object), "_row_id")); !ok || id <= 0 {
			t.Fatalf("annotation %v", m)
		}
	}
	if again, err := apply(2, "transcript.append", payload, nil); err != nil || !pyjson.Equal(again, receipt) {
		t.Fatalf("replay %v %v", again, err)
	}
	_, err = apply(2, "transcript.append", pyjson.ObjectOf("messages", []any{pyjson.ObjectOf("role", "user", "content", "different")}), nil)
	if refusalReason(t, err) != "admission_conflict" {
		t.Fatal(err)
	}
	_, err = apply(3, "transcript.append", payload, func(in *WorkerReceipt) { in.SessionID = "foreign" })
	if refusalReason(t, err) != "permission_denied" {
		t.Fatal(err)
	}
	_, err = apply(3, "transcript.append", payload, func(in *WorkerReceipt) { in.Generation = 1 })
	if refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
	stored, err := db.GetMessages(ctx, "assigned", GetMessagesOptions{})
	if err != nil || len(stored) != 3 {
		t.Fatalf("stored %v %v", stored, err)
	}
	if !pyjson.Equal(rowValue(stored[1], "reasoning"), "private reason") || !pyjson.Equal(rowValue(stored[2], "tool_call_id"), "call1") || !pyjson.Equal(rowValue(stored[2], "content"), "tool-marker") {
		t.Fatalf("stored rows %v", stored)
	}
	_, err = apply(3, "SQL", pyjson.ObjectOf("sql", "DELETE FROM messages"), nil)
	if refusalReason(t, err) != "invalid_params" {
		t.Fatal(err)
	}
	renewed, err := apply(3, "turn.renew", pyjson.ObjectOf("holder", "worker-lease", "ttl_seconds", int64(300)), nil)
	if err != nil || !pyTruthy(resultValue(renewed, "value")) {
		t.Fatalf("renew %v %v", renewed, err)
	}
	released, err := apply(4, "turn.release", pyjson.ObjectOf("holder", "worker-lease"), nil)
	if err != nil || !pyjson.Equal(released, pyjson.ObjectOf("value", nil)) {
		t.Fatalf("release %v %v", released, err)
	}
	assistantContent := []any{pyjson.ObjectOf("type", "text", "text", "canonical winner"+surrogate)}
	winner, err := apply(5, "transcript.append", pyjson.ObjectOf("messages", []any{pyjson.ObjectOf("role", "assistant", "content", assistantContent)}), nil)
	if err != nil {
		t.Fatal(err)
	}
	winnerRow := rowValue(resultValue(winner, "annotations").([]any)[0].(*pyjson.Object), "_row_id")
	repaired, err := apply(6, "transcript.append", pyjson.ObjectOf("messages", []any{pyjson.ObjectOf("role", "assistant", "content", "loser", "_row_id", winnerRow)}), nil)
	if err != nil || !pyjson.Equal(resultValue(repaired, "count"), 0) {
		t.Fatalf("repaired %v %v", repaired, err)
	}
	adopted := resultValue(repaired, "annotations").([]any)[0].(*pyjson.Object)
	canonical, _ := rowValue(adopted, CanonicalRow).(*pyjson.Object)
	if canonical == nil || !pyjson.Equal(rowValue(canonical, "content"), jsonRoundTrip(assistantContent)) {
		t.Fatalf("canonical row %v", adopted)
	}
	if !pyTruthy(rowValue(adopted, DBRowSnapshot)) || !pyjson.Equal(rowValue(adopted, "_row_id"), winnerRow) {
		t.Fatalf("adopted %v", adopted)
	}
	terminal, err := apply(7, "execution.finish", pyjson.NewObject(), nil)
	if err != nil || !pyjson.Equal(resultValue(terminal, "status"), "terminal") {
		t.Fatalf("finish %v %v", terminal, err)
	}
	if again, err := apply(7, "execution.finish", pyjson.NewObject(), nil); err != nil || !pyjson.Equal(again, terminal) {
		t.Fatalf("finish replay %v %v", again, err)
	}
	_, err = apply(8, "usage.main", pyjson.ObjectOf("input_tokens", int64(1)), nil)
	if refusalReason(t, err) != "stale_generation" {
		t.Fatal(err)
	}
}

func TestExplicitWorkerAdoptionPreservesClaimWithoutReexecution(t *testing.T) {
	for _, recoverFirst := range []bool{false, true} {
		ctx := context.Background()
		db := mustSessionDB(t, t.TempDir()+"/state.db", false)
		if err := db.CreateSession(NewSession{ID: "s", Source: "test"}); err != nil {
			t.Fatal(err)
		}
		epoch, _ := db.BeginRuntimeEpoch(ctx, "boot")
		accepted, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "input", Payload: admissionText("work")})
		if err != nil {
			t.Fatal(err)
		}
		claim, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "s"})
		if err != nil || claim == nil {
			t.Fatalf("claim %v %v", claim, err)
		}
		generation, _ := exactInt(rowValue(claim, "generation"))
		assignment := WorkerReceipt{ExecutionID: "compute", SessionID: "s", Generation: generation}
		if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "compute", SessionID: "s", Generation: generation, Kind: "compute", AdoptionSecret: "private"}); err != nil {
			t.Fatal(err)
		}
		newEpoch, _ := db.BeginRuntimeEpoch(ctx, "replacement")
		if recoverFirst {
			if _, err := db.RecoverSessionInputs(ctx, newEpoch); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.AdoptWorkerExecution(ctx, newEpoch, "compute", "s", generation, "private"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RecoverSessionInputs(ctx, newEpoch); err != nil {
			t.Fatal(err)
		}
		admissionID := objStr(accepted, "admission_id")
		restored, _ := db.GetSessionAdmission(ctx, admissionID)
		if objStr(restored, "status") != "started" || !pyjson.Equal(rowValue(restored, "owner_epoch"), newEpoch) || !pyjson.Equal(rowValue(restored, "generation"), generation) {
			t.Fatalf("restored %v", restored)
		}
		if claim, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: newEpoch, SessionID: "s"}); err != nil || claim != nil {
			t.Fatalf("re-execution claim %v %v", claim, err)
		}
		args := appendReceipt(assignment, 1, "committed")
		args.Epoch = newEpoch
		receipt := mustReceipt(t, db, args)
		result, err := db.SettleSessionInput(ctx, SettleInput{Epoch: newEpoch, AdmissionID: admissionID, Generation: generation, Outcome: "completed"})
		if err != nil || objStr(result, "status") != "terminal" {
			t.Fatalf("settle %v %v", result, err)
		}
		before := totalChangesOf(t, db)
		if again := mustReceipt(t, db, args); !pyjson.Equal(again, receipt) {
			t.Fatalf("replay %v", again)
		}
		if totalChangesOf(t, db) != before {
			t.Fatal("replay wrote")
		}
		late := appendReceipt(assignment, 2, "late")
		late.Epoch = newEpoch
		if r := receiptRefusal(t, db, late); r != "stale_generation" {
			t.Fatal(r)
		}
		db.Close()
	}
}

func TestUnknownResolutionAtomicallyRevokesWorkerBeforeUnblocking(t *testing.T) {
	for _, refuseRevoke := range []bool{false, true} {
		ctx := context.Background()
		path := t.TempDir() + "/state.db"
		db := mustSessionDB(t, path, false)
		if err := db.CreateSession(NewSession{ID: "s", Source: "test"}); err != nil {
			t.Fatal(err)
		}
		epoch, _ := db.BeginRuntimeEpoch(ctx, "boot")
		first, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "first", Payload: pyjson.NewObject()})
		if err != nil {
			t.Fatal(err)
		}
		claim, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "s"})
		if err != nil || claim == nil {
			t.Fatalf("claim %v %v", claim, err)
		}
		generation, _ := exactInt(rowValue(claim, "generation"))
		assignment := WorkerReceipt{ExecutionID: "worker", SessionID: "s", Generation: generation}
		if _, err := db.RegisterWorkerExecution(ctx, WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "s", Generation: generation, Kind: "compute", AdoptionSecret: "private"}); err != nil {
			t.Fatal(err)
		}
		follower, err := db.AdmitSessionInput(ctx, SessionInput{Epoch: epoch, PrincipalID: "human", SessionID: "s", RequestID: "next", Payload: pyjson.NewObject()})
		if err != nil {
			t.Fatal(err)
		}
		epoch, _ = db.BeginRuntimeEpoch(ctx, "replacement")
		if _, err := db.RecoverSessionInputs(ctx, epoch); err != nil {
			t.Fatal(err)
		}
		admissionID := objStr(first, "admission_id")
		before := mustSession(t, db, "s")
		if refuseRevoke {
			mustExecConn(t, db.conn, "CREATE TRIGGER refuse_revoke BEFORE UPDATE OF status ON worker_executions BEGIN SELECT RAISE(ABORT, 'revoke refused'); END")
			_, err := db.ResolveUnknownSessionInput(ctx, epoch, admissionID, generation, nil, nil)
			if !isIntegrityError(err) || !strings.Contains(err.Error(), "revoke refused") {
				t.Fatalf("want the revoke integrity error, got %v", err)
			}
			row, _ := db.GetSessionAdmission(ctx, admissionID)
			if objStr(row, "status") != "unknown" || !pyjson.Equal(mustSession(t, db, "s"), before) {
				t.Fatal("refused revoke changed state")
			}
			mustExecConn(t, db.conn, "DROP TRIGGER refuse_revoke")
		}
		result, err := db.ResolveUnknownSessionInput(ctx, epoch, admissionID, generation, nil, nil)
		if err != nil || objStr(result, "status") != "terminal" || objStr(result, "outcome") != "interrupted" {
			t.Fatalf("resolve %v %v", result, err)
		}
		db.Close()
		db = mustSessionDB(t, path, false)
		if _, err := db.AdoptWorkerExecution(ctx, epoch, "worker", "s", generation, "private"); refusalReason(t, err) != "stale_generation" {
			t.Fatal(err)
		}
		late := appendReceipt(assignment, 1, "late")
		late.Epoch = epoch
		if r := receiptRefusal(t, db, late); r != "stale_epoch" {
			t.Fatal(r)
		}
		if len(messageContents(t, db, "s")) != 0 {
			t.Fatal("late append landed")
		}
		claimed, _, err := db.ClaimSessionInput(ctx, ClaimInput{Epoch: epoch, SessionID: "s"})
		if err != nil || claimed == nil || objStr(claimed, "admission_id") != objStr(follower, "admission_id") {
			t.Fatalf("follower claim %v %v", claimed, err)
		}
		db.Close()
	}
}
