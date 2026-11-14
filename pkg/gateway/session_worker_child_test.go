//go:build !windows

package gateway

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Store with a kanban parent of the coder profile, an unrelated cli session and a registered compute worker
func childLedger(t *testing.T) (context.Context, *state.SessionDB, int64) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "parent", Source: "kanban", ProfileName: "coder"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "other", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "boot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RegisterWorkerExecution(ctx, state.WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "parent",
		Kind: "compute", AdoptionSecret: "private-fixture"}); err != nil {
		t.Fatal(err)
	}
	return ctx, db, epoch
}

// Child reservation receipt of the worker execution at a sequence
func reserveChild(t *testing.T, ctx context.Context, db *state.SessionDB, epoch, sequence int64, limits [2]int64, refused *string, executionID, sessionID string) (*pyjson.Object, error) {
	t.Helper()
	identity := map[string]any{"profile_id": "home", "pid": int64(os.Getpid()), "birth": birthOf(t, os.Getpid())}
	handler := func(d *state.SessionDB, ctx context.Context, conn state.SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
		return reserveChildExecution(ctx, d, conn, sid, payload, epoch, executionID, identity, "local", limits, refused)
	}
	return db.ApplyWorkerReceipt(ctx, state.WorkerReceipt{Epoch: epoch, ExecutionID: executionID, SessionID: sessionID, Sequence: sequence,
		Operation: "child.reserve", Payload: pyjson.ObjectOf("secret", fmt.Sprintf("child-secret-%08d", sequence))},
		map[string]state.WorkerHandler{"child.reserve": handler})
}

// Child scope of a successful reservation receipt
func reservedChild(t *testing.T, call func() (*pyjson.Object, error)) *pyjson.Object {
	t.Helper()
	receipt, err := call()
	if err != nil {
		t.Fatal(err)
	}
	child, ok := receipt.GetOr("child", nil).(*pyjson.Object)
	if !ok {
		t.Fatalf("no child in %v", receipt)
	}
	return child
}

// Transcript append of a child execution
func childAppend(ctx context.Context, db *state.SessionDB, epoch int64, child *pyjson.Object, sequence int64, content, sessionID string) error {
	if sessionID == "" {
		sessionID = rowString(child, "session_id")
	}
	_, err := db.MutateWorkerExecution(ctx, state.WorkerReceipt{Epoch: epoch, ExecutionID: rowString(child, "execution_id"), SessionID: sessionID,
		Sequence: sequence, Operation: "transcript.append",
		Payload: pyjson.ObjectOf("messages", []any{pyjson.ObjectOf("role", "assistant", "content", content)})})
	return err
}

// Rows of a read query as column mappings
func readRows(t *testing.T, ctx context.Context, db *state.SessionDB, query string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			row := map[string]any{}
			for i, col := range cols {
				if b, isBytes := values[i].([]byte); isBytes {
					values[i] = string(b)
				}
				row[col] = values[i]
			}
			out = append(out, row)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// Session row of an id
func sessionRow(t *testing.T, ctx context.Context, db *state.SessionDB, sid string) *pyjson.Object {
	t.Helper()
	row, err := db.GetSession(ctx, sid)
	if err != nil || row == nil {
		t.Fatalf("session %s: %v %v", sid, row, err)
	}
	return row
}

func TestOwnerMintsCapsAndScopesChildrenAndRetiresThemWithTheParent(t *testing.T) {
	ctx, db, epoch := childLedger(t)
	limits := [2]int64{2, 1}
	first := reservedChild(t, func() (*pyjson.Object, error) {
		return reserveChild(t, ctx, db, epoch, 1, limits, nil, "worker", "parent")
	})
	second := reservedChild(t, func() (*pyjson.Object, error) {
		return reserveChild(t, ctx, db, epoch, 2, limits, nil, "worker", "parent")
	})
	if receipt, err := reserveChild(t, ctx, db, epoch, 3, limits, nil, "worker", "parent"); err != nil || !pyjson.Equal(receipt, pyjson.ObjectOf("refused", "delegation_capacity")) {
		t.Fatalf("capacity: %v %v", receipt, err)
	}
	draining := "runtime_draining"
	if receipt, err := reserveChild(t, ctx, db, epoch, 4, limits, &draining, "worker", "parent"); err != nil || !pyjson.Equal(receipt, pyjson.ObjectOf("refused", "runtime_draining")) {
		t.Fatalf("draining: %v %v", receipt, err)
	}
	row := sessionRow(t, ctx, db, rowString(first, "session_id"))
	if row.GetOr("source", nil) != "subagent" || row.GetOr("parent_session_id", nil) != "parent" {
		t.Fatalf("child row %v", row)
	}
	kinds := readRows(t, ctx, db, "SELECT kind,status FROM worker_executions WHERE execution_id!='worker'")
	if len(kinds) != 2 || kinds[0]["kind"] != "child" || kinds[0]["status"] != "running" || kinds[1]["kind"] != "child" || kinds[1]["status"] != "running" {
		t.Fatalf("kinds %v", kinds)
	}
	if err := childAppend(ctx, db, epoch, first, 1, "CHILD_OUTPUT", ""); err != nil {
		t.Fatal(err)
	}
	if err := childAppend(ctx, db, epoch, first, 2, "escape", "parent"); !isRuntimeRefusal(err, "permission_denied") {
		t.Fatalf("escape: %v", err)
	}
	messages, err := db.GetMessages(ctx, rowString(first, "session_id"), state.GetMessagesOptions{})
	if err != nil || len(messages) != 1 || messages[0].GetOr("content", nil) != "CHILD_OUTPUT" {
		t.Fatalf("child messages %v %v", messages, err)
	}
	if parent, err := db.GetMessages(ctx, "parent", state.GetMessagesOptions{}); err != nil || len(parent) != 0 {
		t.Fatalf("parent messages %v %v", parent, err)
	}
	unrelated := rowString(first, "session_id") + "-x"
	if err := db.CreateSession(state.NewSession{ID: unrelated, Source: "subagent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MutateWorkerExecution(ctx, state.WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "parent", Sequence: 5,
		Operation: "execution.finish", Payload: pyjson.NewObject()}); err != nil {
		t.Fatal(err)
	}
	if err := childAppend(ctx, db, epoch, second, 1, "late", ""); !isRuntimeRefusal(err, "stale_generation") {
		t.Fatalf("late child write: %v", err)
	}
	for _, row := range readRows(t, ctx, db, "SELECT status FROM worker_executions") {
		if row["status"] != "terminal" {
			t.Fatalf("worker statuses %v", row)
		}
	}
	if reason := sessionRow(t, ctx, db, rowString(second, "session_id")).GetOr("end_reason", nil); reason != "agent_close" {
		t.Fatalf("second end reason %v", reason)
	}
	if ended := sessionRow(t, ctx, db, unrelated).GetOr("ended_at", nil); ended != nil {
		t.Fatalf("unrelated row ended at %v", ended)
	}
}

func TestOwnerRestartRetiresInFlightChildrenInsteadOfRecoveringThem(t *testing.T) {
	ctx, db, epoch := childLedger(t)
	if _, err := db.AdoptWorkerExecution(ctx, epoch, "worker", "parent", 0, "private-fixture"); err != nil {
		t.Fatal(err)
	}
	limits := [2]int64{1, 1}
	reservedChild(t, func() (*pyjson.Object, error) {
		return reserveChild(t, ctx, db, epoch, 1, limits, nil, "worker", "parent")
	})
	children := readRows(t, ctx, db, "SELECT execution_id,session_id FROM worker_executions WHERE kind='child'")
	childEID, childSID := children[0]["execution_id"].(string), children[0]["session_id"].(string)
	if receipt, err := reserveChild(t, ctx, db, epoch, 2, limits, nil, "worker", "parent"); err != nil || !pyjson.Equal(receipt, pyjson.ObjectOf("refused", "delegation_capacity")) {
		t.Fatalf("capacity: %v %v", receipt, err)
	}
	replacement, err := db.BeginRuntimeEpoch(ctx, "restart")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecoverSessionInputs(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	states := map[string]any{}
	for _, row := range readRows(t, ctx, db, "SELECT execution_id,status FROM worker_executions") {
		states[row["execution_id"].(string)] = row["status"]
	}
	if len(states) != 2 || states["worker"] != "unknown" || states[childEID] != "terminal" {
		t.Fatalf("states %v", states)
	}
	if reason := sessionRow(t, ctx, db, childSID).GetOr("end_reason", nil); reason != "agent_close" {
		t.Fatalf("child end reason %v", reason)
	}
	err = childAppend(ctx, db, replacement, pyjson.ObjectOf("execution_id", childEID, "session_id", childSID), 1, "late", "")
	if !isRuntimeRefusal(err, "stale_generation") && !isRuntimeRefusal(err, "stale_epoch") {
		t.Fatalf("late write: %v", err)
	}
}

func TestDepthCapProfileAndAnEarlyChildFinishSettleTheChildRow(t *testing.T) {
	ctx, db, epoch := childLedger(t)
	child := reservedChild(t, func() (*pyjson.Object, error) {
		return reserveChild(t, ctx, db, epoch, 1, [2]int64{2, 1}, nil, "worker", "parent")
	})
	childSID, childEID := rowString(child, "session_id"), rowString(child, "execution_id")
	if profile := sessionRow(t, ctx, db, childSID).GetOr("profile_name", nil); profile != "coder" {
		t.Fatalf("profile %v", profile)
	}
	identity := map[string]any{"profile_id": "home", "pid": int64(os.Getpid()), "birth": birthOf(t, os.Getpid())}
	nested := func(d *state.SessionDB, ctx context.Context, conn state.SQLiteConn, sid string, payload *pyjson.Object) (*pyjson.Object, error) {
		return reserveChildExecution(ctx, d, conn, sid, payload, epoch, childEID, identity, "local", [2]int64{2, 1}, nil)
	}
	receipt, err := db.ApplyWorkerReceipt(ctx, state.WorkerReceipt{Epoch: epoch, ExecutionID: childEID, SessionID: childSID, Sequence: 1,
		Operation: "child.reserve", Payload: pyjson.ObjectOf("secret", "nested-secret-00000001")}, map[string]state.WorkerHandler{"child.reserve": nested})
	if err != nil || !pyjson.Equal(receipt, pyjson.ObjectOf("refused", "delegation_depth_exceeded")) {
		t.Fatalf("nested: %v %v", receipt, err)
	}
	if _, err := db.MutateWorkerExecution(ctx, state.WorkerReceipt{Epoch: epoch, ExecutionID: childEID, SessionID: childSID, Sequence: 2,
		Operation: "session.create", Payload: pyjson.ObjectOf("source", "subagent", "model_config", pyjson.ObjectOf("max_iterations", 7))}); err != nil {
		t.Fatal(err)
	}
	config, err := pyjson.Loads(pyStrOf(sessionRow(t, ctx, db, childSID).GetOr("model_config", nil)))
	if err != nil || !pyjson.Equal(config, pyjson.ObjectOf("_delegate_from", "parent", "max_iterations", 7)) {
		t.Fatalf("model_config %v %v", config, err)
	}
	if _, err := db.MutateWorkerExecution(ctx, state.WorkerReceipt{Epoch: epoch, ExecutionID: childEID, SessionID: childSID, Sequence: 3,
		Operation: "execution.finish", Payload: pyjson.NewObject()}); err != nil {
		t.Fatal(err)
	}
	if reason := sessionRow(t, ctx, db, childSID).GetOr("end_reason", nil); reason != "agent_close" {
		t.Fatalf("child end reason %v", reason)
	}
	if ended := sessionRow(t, ctx, db, "parent").GetOr("ended_at", nil); ended != nil {
		t.Fatalf("parent ended at %v", ended)
	}
}

func TestChildReserveVerifiesTheParentClaimAndRefusesWhileDraining(t *testing.T) {
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "parent", Source: "kanban"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	process := stdinReaderChild(t)
	runner := &authorityRunnerDouble{}
	a := NewSessionAuthority(runner, "p", "owner", db, epoch)
	human := "human"
	a.Sessions["parent"] = NewLiveSession(&SessionSource{Platform: PlatformLocal, UserID: &human}, "route")
	connection := &AuthorityConnection{authority: a, actor: Principal{Subject: "human", ProfileID: "p",
		Capabilities: map[string]bool{"session:submit": true, "worker:adopt": true}}}
	scope := func(extra ...any) *pyjson.Object {
		params := pyjson.ObjectOf("profile_id", "p", "session_id", "parent", "execution_id", "compute", "generation", int64(0),
			"pid", int64(process.Pid), "birth", birthOf(t, process.Pid), "secret", strings.Repeat("w", 32))
		for i := 0; i < len(extra); i += 2 {
			params.Set(extra[i].(string), extra[i+1])
		}
		return params
	}
	registered, err := workerRequest(ctx, connection, SessionRef{ProfileID: "p", SessionID: "parent"}, scope("kind", "compute"), "register")
	if err != nil {
		t.Fatal(err)
	}
	owner := registered.Vals["owner_epoch"]
	reserve := func(sequence int64, extra ...any) (*pyjson.Object, error) {
		args := append(extra, "epoch", owner, "sequence", sequence, "operation", "child.reserve",
			"payload", pyjson.ObjectOf("secret", fmt.Sprintf("child-secret-%08d", sequence)))
		return workerRequest(ctx, connection, SessionRef{}, scope(args...), "persist")
	}
	if _, err := reserve(1, "secret", strings.Repeat("f", 32)); !isRuntimeRefusal(err, "permission_denied") {
		t.Fatalf("forged secret: %v", err)
	}
	runner.draining = true
	if receipt, err := reserve(1); err != nil || !pyjson.Equal(receipt, pyjson.ObjectOf("refused", "runtime_draining")) {
		t.Fatalf("draining: %v %v", receipt, err)
	}
	runner.draining = false
	if receipt, err := reserve(2); err != nil || receipt.GetOr("child", nil) == nil {
		t.Fatalf("reserve: %v %v", receipt, err)
	}
	if rows := readRows(t, ctx, db, "SELECT COUNT(*) AS n FROM worker_executions WHERE kind='child'"); rows[0]["n"] != int64(1) {
		t.Fatalf("children %v", rows)
	}
}
