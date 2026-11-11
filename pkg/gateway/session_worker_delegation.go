package gateway

import (
	"context"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Task keys a delegation dispatch may carry
var delegationTaskKeys = map[string]bool{"goal": true, "goals": true, "context": true, "toolsets": true, "role": true,
	"model": true, "is_batch": true, "task_indexes": true}

// Route keys of a delegation and the ledger column each must match
var delegationRoute = [][2]string{{"session_key", "origin_session"}, {"parent_session_id", "parent_session_id"},
	{"origin_ui_session_id", "origin_ui_session_id"}, {"origin_session_id", "origin_session_id"}}

// Keys a completion event may carry
var delegationEventKeys = func() map[string]bool {
	keys := map[string]bool{}
	for key := range delegationTaskKeys {
		keys[key] = true
	}
	for _, route := range delegationRoute {
		keys[route[0]] = true
	}
	for _, key := range []string{"type", "delegation_id", "status", "completed_at", "dispatched_at", "summary", "error",
		"api_calls", "duration_seconds", "total_duration_seconds", "results", "live_transcripts", "group", "exit_reason",
		"stalled_after_quiet_seconds", "stall_threshold_seconds", "stall_phase", "stall_grace_seconds"} {
		keys[key] = true
	}
	return keys
}()

// Refuses a payload that is not exactly the keys
func delegationShape(payload *pyjson.Object, keys ...string) error {
	if !frameKeysAre(payload, keys...) {
		return runtimeRefusal("invalid_params")
	}
	return nil
}

// Refuses a value that is not a non empty string of at most 512 characters
func delegationText(value any) error {
	text, isText := value.(string)
	if !isText || text == "" || pystr.Len(text) > 512 {
		return runtimeRefusal("invalid_params")
	}
	return nil
}

// Refuses a value that is not a finite non negative int or float
func delegationNumber(value any) error {
	number, ok := strictNumber(value)
	if !ok || math.IsInf(number, 0) || math.IsNaN(number) || number < 0 {
		return runtimeRefusal("invalid_params")
	}
	return nil
}

// Delegation row the session's execution owns
func delegationOwned(ctx context.Context, conn state.SQLiteConn, sessionID, executionID string, delegationID any) (map[string]any, error) {
	if err := delegationText(delegationID); err != nil {
		return nil, err
	}
	row, err := state.ConnRow(ctx, conn, "SELECT * FROM async_delegations WHERE delegation_id=?", delegationID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, runtimeRefusal("not_found")
	}
	if !pyrepr.Equal(row["parent_session_id"], sessionID) || !pyrepr.Equal(row["owner_execution_id"], executionID) {
		return nil, runtimeRefusal("permission_denied")
	}
	return row, nil
}

// Value receipt of a delegation handler
func delegationValue(value any) *pyjson.Object { return pyjson.ObjectOf("value", value) }

// Text of a nullable column with NULL as empty
func nullColumnText(value any) any {
	if value == nil {
		return ""
	}
	return value
}

// Records a dispatched delegation owned by the verified worker execution and process
func delegationDispatch(ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object, executionID string, workerPID int, workerBirth float64) (*pyjson.Object, error) {
	keys := []string{"delegation_id", "task", "dispatched_at"}
	for _, route := range delegationRoute {
		keys = append(keys, route[0])
	}
	if err := delegationShape(payload, keys...); err != nil {
		return nil, err
	}
	if err := delegationText(payload.Vals["delegation_id"]); err != nil {
		return nil, err
	}
	if err := delegationNumber(payload.Vals["dispatched_at"]); err != nil {
		return nil, err
	}
	task, isDict := payload.Vals["task"].(*pyjson.Object)
	if !isDict {
		return nil, runtimeRefusal("invalid_params")
	}
	for _, key := range task.Keys {
		if !delegationTaskKeys[key] {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	session, err := state.ConnRow(ctx, conn, "SELECT source,session_key FROM sessions WHERE id=?", sessionID)
	if err != nil {
		return nil, err
	}
	if session == nil || !map[any]bool{"cli": true, "tui": true, "gui": true, "tool": true, "oneshot": true}[session["source"]] {
		return nil, runtimeRefusal("unsupported_producer")
	}
	if !pyrepr.Equal(payload.Vals["parent_session_id"], sessionID) || !pyrepr.Equal(payload.Vals["session_key"], nullColumnText(session["session_key"])) ||
		!(payload.Vals["origin_ui_session_id"] == "" || pyrepr.Equal(payload.Vals["origin_ui_session_id"], sessionID)) ||
		!(payload.Vals["origin_session_id"] == "" || pyrepr.Equal(payload.Vals["origin_session_id"], sessionID)) {
		return nil, runtimeRefusal("permission_denied")
	}
	old, err := state.ConnRow(ctx, conn, "SELECT 1 FROM async_delegations WHERE delegation_id=?", payload.Vals["delegation_id"])
	if err != nil {
		return nil, err
	}
	if old != nil {
		if _, err := delegationOwned(ctx, conn, sessionID, executionID, payload.Vals["delegation_id"]); err != nil {
			return nil, err
		}
		return nil, runtimeRefusal("admission_conflict")
	}
	birth, err := proc.WorkerBirth(workerPID)
	if err != nil {
		return nil, runtimeRefusal("worker_not_live")
	}
	if birth != workerBirth {
		return nil, runtimeRefusal("permission_denied")
	}
	ownerStarted := GetProcessStartTime(workerPID)
	taskJSON, err := pyjson.Dumps(task)
	if err != nil {
		return nil, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO async_delegations
        (delegation_id,origin_session,origin_ui_session_id,parent_session_id,state,dispatched_at,
         updated_at,owner_pid,owner_started_at,task_json,origin_session_id,owner_execution_id)
        VALUES(?,?,?,?,'running',?,?,?,?,?,?,?)`, payload.Vals["delegation_id"], payload.Vals["session_key"],
		payload.Vals["origin_ui_session_id"], sessionID, payload.Vals["dispatched_at"], nowSeconds(), workerPID, ownerStarted,
		taskJSON, payload.Vals["origin_session_id"], executionID)
	if err != nil {
		return nil, err
	}
	return delegationValue(nil), nil
}

// Wall clock seconds as Python time.time
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Finishes a running delegation with its completion event and result
func delegationComplete(ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object, executionID string) (*pyjson.Object, error) {
	if err := delegationShape(payload, "event", "result"); err != nil {
		return nil, err
	}
	event, eventDict := payload.Vals["event"].(*pyjson.Object)
	_, resultDict := payload.Vals["result"].(*pyjson.Object)
	if !eventDict || !resultDict || event.GetOr("type", nil) != "async_delegation" {
		return nil, runtimeRefusal("invalid_params")
	}
	for _, key := range event.Keys {
		if !delegationEventKeys[key] {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	switch event.GetOr("status", nil) {
	case "completed", "success", "error", "interrupted", "stalled", "unknown":
	default:
		return nil, runtimeRefusal("invalid_params")
	}
	row, err := delegationOwned(ctx, conn, sessionID, executionID, event.GetOr("delegation_id", nil))
	if err != nil {
		return nil, err
	}
	for _, route := range delegationRoute {
		if !pyrepr.Equal(event.GetOr(route[0], ""), nullColumnText(row[route[1]])) {
			return nil, runtimeRefusal("permission_denied")
		}
	}
	if err := delegationNumber(event.GetOr("completed_at", nil)); err != nil {
		return nil, err
	}
	if row["state"] != "running" {
		return nil, runtimeRefusal("admission_conflict")
	}
	eventJSON, err := pyjson.Dumps(event)
	if err != nil {
		return nil, err
	}
	resultJSON, err := pyjson.Dumps(payload.Vals["result"])
	if err != nil {
		return nil, err
	}
	_, err = conn.ExecContext(ctx, `UPDATE async_delegations SET state=?,completed_at=?,updated_at=?,event_json=?,result_json=?
        WHERE delegation_id=?`, event.Vals["status"], event.Vals["completed_at"], nowSeconds(), eventJSON, resultJSON, event.Vals["delegation_id"])
	if err != nil {
		return nil, err
	}
	return delegationValue(nil), nil
}

// Records one child's partial result entry on a running delegation
func delegationChild(ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object, executionID string) (*pyjson.Object, error) {
	if err := delegationShape(payload, "delegation_id", "entry"); err != nil {
		return nil, err
	}
	row, err := delegationOwned(ctx, conn, sessionID, executionID, payload.Vals["delegation_id"])
	if err != nil {
		return nil, err
	}
	entry, isDict := payload.Vals["entry"].(*pyjson.Object)
	if !isDict {
		return nil, runtimeRefusal("invalid_params")
	}
	index, isInt := strictInt(entry.GetOr("task_index", nil))
	if !isInt || index.Sign() < 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	if row["state"] != "running" {
		return delegationValue(nil), nil
	}
	text := "{}"
	if stored, ok := row["result_json"].(string); ok && stored != "" {
		text = stored
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	previous, _ := decoded.(*pyjson.Object)
	var results []any
	if previous != nil {
		items, _ := previous.GetOr("results", []any{}).([]any)
		for _, item := range items {
			kept, _ := item.(*pyjson.Object)
			if kept == nil || !pyrepr.Equal(kept.GetOr("task_index", nil), entry.Vals["task_index"]) {
				results = append(results, item)
			}
		}
	}
	results = append(results, entry)
	encoded, err := pyjson.Dumps(pyjson.ObjectOf("results", results, "partial", true))
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE async_delegations SET result_json=?,updated_at=? WHERE delegation_id=?",
		encoded, nowSeconds(), payload.Vals["delegation_id"]); err != nil {
		return nil, err
	}
	return delegationValue(nil), nil
}

// Delegation state and result the owning execution may read
func delegationRead(ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object, executionID string) (*pyjson.Object, error) {
	if err := delegationShape(payload, "delegation_id"); err != nil {
		return nil, err
	}
	row, err := delegationOwned(ctx, conn, sessionID, executionID, payload.Vals["delegation_id"])
	if err != nil {
		return nil, err
	}
	value := pyjson.NewObject()
	for _, key := range []string{"delegation_id", "origin_session", "state", "dispatched_at", "completed_at", "delivery_state",
		"delivery_attempts", "origin_session_id"} {
		value.Set(key, row[key])
	}
	var result any
	if text, ok := row["result_json"].(string); ok && text != "" {
		if result, err = pyjson.Loads(text); err != nil {
			return nil, err
		}
	}
	value.Set("result", result)
	return delegationValue(value), nil
}

// Claims a finished delegation's pending delivery for one claimant, a stale claim older than five minutes taken over
func delegationClaim(ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object, executionID string) (*pyjson.Object, error) {
	if err := delegationShape(payload, "delegation_id", "claim_id"); err != nil {
		return nil, err
	}
	row, err := delegationOwned(ctx, conn, sessionID, executionID, payload.Vals["delegation_id"])
	if err != nil {
		return nil, err
	}
	if err := delegationText(payload.Vals["claim_id"]); err != nil {
		return nil, err
	}
	now := nowSeconds()
	if row["state"] == "running" || row["state"] == "finalizing" {
		return delegationValue(false), nil
	}
	result, err := conn.ExecContext(ctx, `UPDATE async_delegations SET delivery_claim=?,delivery_claimed_at=?,
        delivery_attempts=delivery_attempts+1,updated_at=? WHERE delegation_id=? AND delivery_state='pending'
        AND (delivery_claim IS NULL OR delivery_claimed_at < ?)`, payload.Vals["claim_id"], now, now, payload.Vals["delegation_id"], now-300)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	return delegationValue(changed == 1), nil
}

// Closed delivery transitions of a claimed delegation, each a constant statement fragment and its bound values
var delegationDeliveryUpdates = map[string]func(now float64) (string, []any){
	"ack":   func(now float64) (string, []any) { return "delivery_state='delivered',delivered_at=?", []any{now} },
	"drop":  func(float64) (string, []any) { return "delivery_state='dropped'", nil },
	"defer": func(float64) (string, []any) { return "delivery_attempts=MAX(0,delivery_attempts-1)", nil },
	"release": func(float64) (string, []any) {
		return "delivery_state=CASE WHEN delivery_attempts>=? THEN 'dropped' ELSE delivery_state END", []any{tools.AsyncMaxDeliveryAttempts}
	},
}

// Applies one delivery transition to the claim holder's pending delegation
func delegationDelivery(ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object, executionID, action string) (*pyjson.Object, error) {
	if err := delegationShape(payload, "delegation_id", "claim_id"); err != nil {
		return nil, err
	}
	if _, err := delegationOwned(ctx, conn, sessionID, executionID, payload.Vals["delegation_id"]); err != nil {
		return nil, err
	}
	if err := delegationText(payload.Vals["claim_id"]); err != nil {
		return nil, err
	}
	now := nowSeconds()
	fragment, args := delegationDeliveryUpdates[action](now)
	args = append(args, now, payload.Vals["delegation_id"], payload.Vals["claim_id"])
	result, err := conn.ExecContext(ctx, "UPDATE async_delegations SET "+fragment+
		",updated_at=?,delivery_claim=NULL,delivery_claimed_at=NULL WHERE delegation_id=? AND delivery_state='pending' AND delivery_claim=?", args...)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	return delegationValue(changed == 1), nil
}

// Withdraws a running delegation that never got scheduled and holds no result
func delegationUnscheduled(ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object, executionID string) (*pyjson.Object, error) {
	if err := delegationShape(payload, "delegation_id"); err != nil {
		return nil, err
	}
	row, err := delegationOwned(ctx, conn, sessionID, executionID, payload.Vals["delegation_id"])
	if err != nil {
		return nil, err
	}
	if row["state"] != "running" || row["result_json"] != nil {
		return nil, runtimeRefusal("admission_conflict")
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM async_delegations WHERE delegation_id=?", payload.Vals["delegation_id"]); err != nil {
		return nil, err
	}
	return delegationValue(nil), nil
}

// Delegation ledger handlers bound to a verified worker execution and process
func workerDelegationHandlers(executionID any, workerPID any, workerBirth any) (map[string]state.WorkerHandler, error) {
	if err := delegationText(executionID); err != nil {
		return nil, err
	}
	pid, isInt := strictInt(workerPID)
	if !isInt || pid.Sign() <= 0 || !pid.IsInt64() {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := delegationNumber(workerBirth); err != nil {
		return nil, err
	}
	execution := executionID.(string)
	birth, _ := strictNumber(workerBirth)
	bind := func(fn func(context.Context, state.SQLiteConn, string, *pyjson.Object, string) (*pyjson.Object, error)) state.WorkerHandler {
		return func(_ *state.SessionDB, ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
			return fn(ctx, conn, sessionID, payload, execution)
		}
	}
	handlers := map[string]state.WorkerHandler{
		"delegation.complete": bind(delegationComplete), "delegation.child": bind(delegationChild), "delegation.read": bind(delegationRead),
		"delegation.claim": bind(delegationClaim), "delegation.unscheduled": bind(delegationUnscheduled),
		"delegation.dispatch": func(_ *state.SessionDB, ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
			return delegationDispatch(ctx, conn, sessionID, payload, execution, int(pid.Int64()), birth)
		},
	}
	for action := range delegationDeliveryUpdates {
		handlers["delegation."+action] = func(_ *state.SessionDB, ctx context.Context, conn state.SQLiteConn, sessionID string, payload *pyjson.Object) (*pyjson.Object, error) {
			return delegationDelivery(ctx, conn, sessionID, payload, execution, action)
		}
	}
	return handlers, nil
}

// Delegation ledger sibling of the worker mutation sharing its receipt stream, called after the claim verification
func persistWorkerDelegation(ctx context.Context, db *state.SessionDB, in state.WorkerReceipt, workerPID, workerBirth any) (*pyjson.Object, error) {
	handlers, err := workerDelegationHandlers(in.ExecutionID, workerPID, workerBirth)
	if err != nil {
		return nil, err
	}
	return db.ApplyWorkerReceipt(ctx, in, handlers)
}

// Worker persist delegation family with every authentication step mandatory, a completion queued for delivery
func workerDelegationRequest(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (*pyjson.Object, error) {
	if !workerParamsAre(params, "epoch", "sequence", "operation", "payload") {
		return nil, runtimeRefusal("invalid_params")
	}
	claim, err := workerClaim(ctx, c, params)
	if err != nil {
		return nil, err
	}
	a := c.authority
	if !c.actor.Capabilities["worker:adopt"] {
		return nil, runtimeRefusal("permission_denied")
	}
	if err := verifyWorkerClaim(ctx, c, params, claim); err != nil {
		return nil, err
	}
	executionID, err := ledgerText(params.Vals["execution_id"])
	if err != nil {
		return nil, err
	}
	sessionID, err := ledgerText(params.Vals["session_id"])
	if err != nil {
		return nil, err
	}
	named, _ := params.Vals["operation"].(string)
	result, err := persistWorkerDelegation(ctx, a.DB, state.WorkerReceipt{Epoch: workerLedgerInt(params.Vals["epoch"]), ExecutionID: executionID,
		SessionID: sessionID, Generation: workerLedgerInt(params.Vals["generation"]), Sequence: max(workerLedgerInt(params.Vals["sequence"]), 0),
		Operation: named, Payload: params.Vals["payload"]}, params.Vals["pid"], params.Vals["birth"])
	if err != nil {
		return nil, err
	}
	if named != "delegation.complete" {
		return result, nil
	}
	var event *pyjson.Object
	err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		payload, _ := params.Vals["payload"].(*pyjson.Object)
		completion, _ := payload.GetOr("event", nil).(*pyjson.Object)
		row, err := delegationOwned(ctx, conn, ref.SessionID, executionID, completion.GetOr("delegation_id", nil))
		if err != nil {
			return err
		}
		if row["delivery_state"] == "pending" {
			decoded, err := pyjson.Loads(pyStrOf(row["event_json"]))
			if err != nil {
				return err
			}
			event, _ = decoded.(*pyjson.Object)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if event != nil {
		queued, _ := pyjson.Plain(event).(map[string]any)
		queued["restored"] = true
		tools.DefaultProcessRegistry().CompletionQueue.Put(queued)
	}
	return result, nil
}
