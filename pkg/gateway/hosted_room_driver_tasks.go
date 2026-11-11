package gateway

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Acquires an empty or expired room lease under a monotonic generation
func DriverAcquireLease(ctx context.Context, dbPath string, roomIDValue, gatewayIDValue, authorityEpoch, processGenerationValue, ttlSeconds any, clock DriverClock) (DriverLease, error) {
	roomID, err := driverIdentifier(roomIDValue, "room_id")
	if err != nil {
		return DriverLease{}, err
	}
	gatewayID, err := driverIdentifier(gatewayIDValue, "gateway_id")
	if err != nil {
		return DriverLease{}, err
	}
	epochValue, err := roomBoundedInt(authorityEpoch, driverValidation, "authority_epoch must be a positive integer", 1, nil)
	if err != nil {
		return DriverLease{}, err
	}
	epoch := epochValue.Int64()
	processGeneration, err := driverIdentifier(processGenerationValue, "process_generation")
	if err != nil {
		return DriverLease{}, err
	}
	now, expiresAt, err := driverLeaseWindow(ttlSeconds, clock)
	if err != nil {
		return DriverLease{}, err
	}
	var out DriverLease
	err = driverTransaction(ctx, dbPath, func(conn state.SQLiteConn) error {
		if _, err := driverRequireRoomAuthority(ctx, conn, roomID, gatewayID, epoch); err != nil {
			return err
		}
		row, err := state.QueryRowObject(ctx, conn, driverSelectLease, roomID)
		if err != nil {
			return err
		}
		if row == nil {
			if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_driver_leases (
                       room_id, gateway_id, authority_epoch, process_generation, lease_generation,
                       expires_at, acquired_at, updated_at, released_at
                   ) VALUES (?, ?, ?, ?, 1, ?, ?, ?, NULL)`, roomID, gatewayID, epoch, processGeneration, expiresAt, now, now); err != nil {
				return err
			}
			inserted, err := state.QueryRowObject(ctx, conn, driverSelectLease, roomID)
			if err != nil {
				return err
			}
			out = driverLeaseFromRow(inserted, false)
			return nil
		}
		sameAuthority := hrStr(row, "gateway_id") == gatewayID && hrInt(row, "authority_epoch") == epoch
		live := row.Vals["released_at"] == nil && hrFloat(row, "expires_at") > now
		if sameAuthority && hrStr(row, "process_generation") == processGeneration && live {
			renewed := max(hrFloat(row, "expires_at"), expiresAt)
			if _, err := conn.ExecContext(ctx, "UPDATE hosted_room_driver_leases SET expires_at=?, updated_at=? WHERE room_id=? AND lease_generation=?",
				renewed, now, roomID, hrInt(row, "lease_generation")); err != nil {
				return err
			}
			out = driverLeaseFromRow(row, false)
			out.ExpiresAt = renewed
			return nil
		}
		if sameAuthority && live {
			return leaseHeld("room driver lease is held by another generation")
		}
		if err := roomFencedUpdate(ctx, conn, `UPDATE hosted_room_driver_leases
               SET gateway_id=?, authority_epoch=?, process_generation=?, lease_generation=lease_generation + 1,
                   expires_at=?, acquired_at=?, updated_at=?, released_at=NULL
               WHERE room_id=? AND lease_generation=? AND (
                   gateway_id != ? OR authority_epoch != ? OR released_at IS NOT NULL OR expires_at <= ?)`,
			[]any{gatewayID, epoch, processGeneration, expiresAt, now, now, roomID, hrInt(row, "lease_generation"), gatewayID, epoch, now},
			leaseHeld("room driver lease changed during acquisition")); err != nil {
			return err
		}
		acquired, err := state.QueryRowObject(ctx, conn, driverSelectLease, roomID)
		if err != nil {
			return err
		}
		out = driverLeaseFromRow(acquired, true)
		return nil
	})
	return out, err
}

// Renews the exact active lease generation or fails closed
func DriverRenewLease(ctx context.Context, dbPath string, lease DriverLease, ttlSeconds any, clock DriverClock) (DriverLease, error) {
	now, requested, err := driverLeaseWindow(ttlSeconds, clock)
	if err != nil {
		return DriverLease{}, err
	}
	var out DriverLease
	err = driverTransaction(ctx, dbPath, func(conn state.SQLiteConn) error {
		current, err := driverRequireActiveLease(ctx, conn, lease, now)
		if err != nil {
			return err
		}
		expiresAt := max(hrFloat(current, "expires_at"), requested)
		args := append(append([]any{expiresAt, now, lease.RoomID}, driverRunFenceArgs(lease)...), now)
		if err := roomFencedUpdate(ctx, conn, `UPDATE hosted_room_driver_leases SET expires_at=?, updated_at=?
               WHERE room_id=? AND gateway_id=? AND process_generation=?
                 AND lease_generation=? AND released_at IS NULL AND expires_at > ?`, args, staleLease("driver lease changed during renewal")); err != nil {
			return err
		}
		out = lease
		out.ExpiresAt, out.Reclaimed = expiresAt, false
		return nil
	})
	return out, err
}

// Releases the exact active lease generation idempotently
func DriverReleaseLease(ctx context.Context, dbPath string, lease DriverLease, clock DriverClock) (map[string]any, error) {
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = driverTransaction(ctx, dbPath, func(conn state.SQLiteConn) error {
		if _, err := driverRequireRoomAuthority(ctx, conn, lease.RoomID, lease.GatewayID, lease.AuthorityEpoch); err != nil {
			return err
		}
		row, err := state.QueryRowObject(ctx, conn, driverSelectLease, lease.RoomID)
		if err != nil {
			return err
		}
		if !driverLeaseRowMatches(row, lease) {
			return staleLease("driver lease is stale")
		}
		if row.Vals["released_at"] != nil {
			out = map[string]any{"lease": driverLeaseFromRow(row, false), "idempotent": true}
			return nil
		}
		if hrFloat(row, "expires_at") <= now {
			return staleLease("driver lease expired before release")
		}
		running, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM hosted_room_driver_tasks WHERE room_id=? AND status='running' LIMIT 1", lease.RoomID)
		if err != nil {
			return err
		}
		if running != nil {
			return invalidTransition("cannot release a room lease while tasks are running")
		}
		if _, err := conn.ExecContext(ctx, `UPDATE hosted_room_driver_leases SET expires_at=?, updated_at=?, released_at=?
               WHERE room_id=? AND lease_generation=?`, now, now, now, lease.RoomID, lease.LeaseGeneration); err != nil {
			return err
		}
		released := driverLeaseFromRow(row, false)
		released.ExpiresAt = now
		out = map[string]any{"lease": released, "idempotent": false}
		return nil
	})
	return out, err
}

// Persists a queued task, or returns the identical admission
func DriverAdmitTask(ctx context.Context, dbPath string, identity TaskIdentity, payload any, clock DriverClock) (map[string]any, error) {
	normalized, payloadJSON, digest, err := driverTaskPayload(payload)
	if err != nil {
		return nil, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = driverTransaction(ctx, dbPath, func(conn state.SQLiteConn) error {
		if _, err := driverLoadActiveRoom(ctx, conn, identity.RoomID); err != nil {
			return err
		}
		existing, err := driverLoadTask(ctx, conn, identity, false)
		if err != nil {
			return err
		}
		if existing != nil {
			if hrStr(existing, "payload_digest") != digest || hrStr(existing, "payload_json") != payloadJSON {
				return taskConflict("task_id is already bound to a different payload")
			}
			out, err = driverTaskFromRow(existing, true)
			return err
		}
		bound, err := state.QueryRowObject(ctx, conn, "SELECT * FROM hosted_room_driver_tasks WHERE room_id=? AND thread_id=? AND turn_id=?",
			identity.RoomID, identity.ThreadID, identity.TurnID)
		if err != nil {
			return err
		}
		if bound != nil {
			return taskConflict("thread_id and turn_id are already bound to a task")
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_driver_tasks (
                   room_id, task_id, thread_id, turn_id, source_event_seq, payload_json, payload_digest,
                   status, execution_generation, cancel_generation, created_at, updated_at
               ) VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', 0, 0, ?, ?)`, identity.RoomID, identity.TaskID, identity.ThreadID, identity.TurnID,
			driverSQLInt(normalized["source_event_seq"]), payloadJSON, digest, now, now); err != nil {
			return err
		}
		if err := captureTransitionLocked(ctx, conn, identity.RoomID); err != nil {
			return err
		}
		row, err := driverLoadTask(ctx, conn, identity, true)
		if err != nil {
			return err
		}
		out, err = driverTaskFromRow(row, false)
		return err
	})
	return out, err
}

// SQLite bind value of a normalized integer, a big one bound as its decimal text so SQLite raises its overflow
func driverSQLInt(value any) any {
	if n, ok := value.(*big.Int); ok {
		return n.String()
	}
	return value
}

// Moves one queued task to running under the current driver lease
func DriverStartTask(ctx context.Context, dbPath string, identity TaskIdentity, lease DriverLease, expectedCancelGeneration int64, clock DriverClock) (TaskAttempt, error) {
	if err := driverCheckSameRoom(lease, identity); err != nil {
		return TaskAttempt{}, err
	}
	if err := driverCancelGeneration(expectedCancelGeneration); err != nil {
		return TaskAttempt{}, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return TaskAttempt{}, err
	}
	var out TaskAttempt
	err = driverTransaction(ctx, dbPath, func(conn state.SQLiteConn) error {
		if _, err := driverRequireActiveLease(ctx, conn, lease, now); err != nil {
			return err
		}
		row, err := driverLoadTask(ctx, conn, identity, true)
		if err != nil {
			return err
		}
		if err := driverRequireCancelGeneration(row, expectedCancelGeneration); err != nil {
			return err
		}
		if status := hrStr(row, "status"); status != "queued" {
			return invalidTransition(fmt.Sprintf("cannot start task in state '%s'", status))
		}
		prior, err := state.QueryRowObject(ctx, conn, `SELECT task_id, status FROM hosted_room_driver_tasks
               WHERE room_id=? AND status IN ('running', 'indeterminate', 'stopping') `+driverTaskOrder+" LIMIT 1", identity.RoomID)
		if err != nil {
			return err
		}
		if prior != nil {
			return invalidTransition("room recovery must resolve the prior task before starting new work")
		}
		next, err := state.QueryRowObject(ctx, conn, "SELECT task_id FROM hosted_room_driver_tasks WHERE room_id=? AND status='queued' "+driverTaskOrder+" LIMIT 1", identity.RoomID)
		if err != nil {
			return err
		}
		if next == nil || hrStr(next, "task_id") != identity.TaskID {
			return invalidTransition("task is not next in the hosted room event order")
		}
		executionGeneration := hrInt(row, "execution_generation") + 1
		args := append(append([]any{executionGeneration}, driverRunFenceArgs(lease)...), now, now, identity.RoomID, identity.TaskID, expectedCancelGeneration)
		if err := roomFencedUpdate(ctx, conn, `UPDATE hosted_room_driver_tasks
               SET status='running', execution_generation=?, run_gateway_id=?, run_process_generation=?,
                   run_lease_generation=?, started_at=?, updated_at=?
               WHERE room_id=? AND task_id=? AND status='queued' AND cancel_generation=?`, args, staleTask("task changed during start")); err != nil {
			return err
		}
		if err := captureTransitionLocked(ctx, conn, identity.RoomID); err != nil {
			return err
		}
		out = TaskAttempt{Identity: identity, Lease: lease, ExecutionGeneration: executionGeneration, CancelGeneration: expectedCancelGeneration}
		return nil
	})
	return out, err
}

// Commits one terminal result while every lease and task fence still matches
func DriverSettleTask(ctx context.Context, dbPath string, attempt TaskAttempt, settlementID any, status string, result any, clock DriverClock) (map[string]any, error) {
	now, replay, setParams, err := driverSettlement(settlementID, status, result, clock)
	if err != nil {
		return nil, err
	}
	return driverRunFenceTransition(ctx, dbPath, attempt, "task attempt is stale or cancelled", false,
		driverTransitionSpec{leaseAfter: true, now: now, replay: replay, sql: driverSettleRunningSQL, setParams: setParams, stale: "task changed during settlement"})
}

// Commits a completion that won the race with an unacknowledged Stop
func DriverSettleStoppingTask(ctx context.Context, dbPath string, identity TaskIdentity, lease DriverLease, expectedExecutionGeneration, expectedCancelGeneration int64,
	settlementID any, status string, result any, clock DriverClock) (map[string]any, error) {
	if _, err := driverTerminalSettlementID(settlementID, status); err != nil {
		return nil, err
	}
	if expectedExecutionGeneration < 1 || expectedCancelGeneration < 1 {
		return nil, driverValidation("stopping settlement generations are invalid")
	}
	now, replay, setParams, err := driverSettlement(settlementID, status, result, clock)
	if err != nil {
		return nil, err
	}
	return driverTransition(ctx, dbPath, identity, driverTransitionSpec{lease: &lease, leaseAfter: true, now: now, replay: replay, sql: driverSettleStoppingSQL,
		setParams: setParams, fenceParams: []any{expectedExecutionGeneration, expectedCancelGeneration}, stale: "task completion lost the stop race"})
}

// Commits a verified historical receipt under the current room lease
func DriverResolveIndeterminateTask(ctx context.Context, dbPath string, identity TaskIdentity, lease DriverLease, expectedExecutionGeneration, expectedCancelGeneration int64,
	settlementID any, status string, result any, clock DriverClock) (map[string]any, error) {
	if err := driverExpectedGenerations(lease, identity, expectedExecutionGeneration, expectedCancelGeneration); err != nil {
		return nil, err
	}
	now, replay, setParams, err := driverSettlement(settlementID, status, result, clock)
	if err != nil {
		return nil, err
	}
	return driverGenerationTransition(ctx, dbPath, identity, lease, "resolve", expectedExecutionGeneration, expectedCancelGeneration, now, setParams, replay)
}

// Commits a verified terminal cancellation of an uncertain attempt
func DriverResolveIndeterminateCancellation(ctx context.Context, dbPath string, identity TaskIdentity, lease DriverLease, expectedExecutionGeneration, expectedCancelGeneration int64,
	cancelIDValue any, clock DriverClock) (map[string]any, error) {
	if err := driverExpectedGenerations(lease, identity, expectedExecutionGeneration, expectedCancelGeneration); err != nil {
		return nil, err
	}
	cancelID, err := driverIdentifier(cancelIDValue, "cancel_id")
	if err != nil {
		return nil, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	return driverGenerationTransition(ctx, dbPath, identity, lease, "resolve_cancel", expectedExecutionGeneration, expectedCancelGeneration, now,
		[]any{expectedCancelGeneration + 1, cancelID, now, now}, driverCancelReplay(cancelID, "cancelled"))
}

// Retries uncertain work after an operator accepts at least once risk
func DriverRequeueIndeterminateTask(ctx context.Context, dbPath string, identity TaskIdentity, lease DriverLease, expectedExecutionGeneration, expectedCancelGeneration int64, clock DriverClock) (map[string]any, error) {
	if err := driverExpectedGenerations(lease, identity, expectedExecutionGeneration, expectedCancelGeneration); err != nil {
		return nil, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	return driverGenerationTransition(ctx, dbPath, identity, lease, "requeue", expectedExecutionGeneration, expectedCancelGeneration, now, []any{now}, nil)
}

// Fences one uncertain attempt and releases later room work
func DriverDeferIndeterminateTask(ctx context.Context, dbPath string, identity TaskIdentity, lease DriverLease, expectedExecutionGeneration, expectedCancelGeneration int64,
	reasonValue any, clock DriverClock) (map[string]any, error) {
	if err := driverExpectedGenerations(lease, identity, expectedExecutionGeneration, expectedCancelGeneration); err != nil {
		return nil, err
	}
	reason, err := driverIdentifier(reasonValue, "defer_reason")
	if err != nil {
		return nil, err
	}
	resultJSON, err := roomCanonicalJSON(map[string]any{"reason": reason, "retryable": true}, "result", driverMaxResultJSONBytes, true, driverValidation)
	if err != nil {
		return nil, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	replay := func(row *pyjson.Object) (map[string]any, error) {
		if driverGenerationsMatch(row, "deferred", expectedExecutionGeneration, expectedCancelGeneration) && row.Vals["result_json"] != nil && hrStr(row, "result_json") == resultJSON {
			return driverTaskFromRow(row, true)
		}
		return nil, nil
	}
	return driverGenerationTransition(ctx, dbPath, identity, lease, "defer", expectedExecutionGeneration, expectedCancelGeneration, now, []any{resultJSON, now, now}, replay)
}

// Retries a fenced deferred turn under a new generation
func DriverRequeueDeferredTask(ctx context.Context, dbPath string, identity TaskIdentity, lease DriverLease, expectedExecutionGeneration, expectedCancelGeneration int64, clock DriverClock) (map[string]any, error) {
	if err := driverExpectedGenerations(lease, identity, expectedExecutionGeneration, expectedCancelGeneration); err != nil {
		return nil, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	return driverGenerationTransition(ctx, dbPath, identity, lease, "requeue_deferred", expectedExecutionGeneration, expectedCancelGeneration, now, []any{now}, nil)
}

// Returns a running task to its durable queue after proven non admission
func DriverRequeueNotAdmittedTask(ctx context.Context, dbPath string, attempt TaskAttempt, clock DriverClock) (map[string]any, error) {
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	if err := driverCheckSameRoom(attempt.Lease, attempt.Identity); err != nil {
		return nil, err
	}
	replay := func(row *pyjson.Object) (map[string]any, error) {
		if driverGenerationsMatch(row, "queued", attempt.ExecutionGeneration, attempt.CancelGeneration) &&
			row.Vals["run_gateway_id"] == nil && row.Vals["run_process_generation"] == nil && row.Vals["run_lease_generation"] == nil {
			return driverTaskFromRow(row, true)
		}
		return nil, nil
	}
	return driverRunFenceTransition(ctx, dbPath, attempt, "not-admitted task attempt lost its fence", true,
		driverTransitionSpec{now: now, replay: replay, sql: driverRequeueRunningSQL, setParams: []any{now}, stale: "not-admitted task changed during requeue"})
}

// Cancels a queued task before any external work was admitted
func DriverCancelTask(ctx context.Context, dbPath string, identity TaskIdentity, cancelIDValue any, expectedCancelGeneration int64, clock DriverClock) (map[string]any, error) {
	cancelID, err := driverIdentifier(cancelIDValue, "cancel_id")
	if err != nil {
		return nil, err
	}
	if err := driverCancelGeneration(expectedCancelGeneration); err != nil {
		return nil, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	guard := func(row *pyjson.Object) error {
		status := hrStr(row, "status")
		if DriverTerminalStatuses[status] {
			return invalidTransition(fmt.Sprintf("cannot cancel task in state '%s'", status))
		}
		if status != "queued" && status != "deferred" {
			return invalidTransition("running work requires acknowledged two-phase cancellation")
		}
		return driverRequireCancelGeneration(row, expectedCancelGeneration)
	}
	return driverTransition(ctx, dbPath, identity, driverTransitionSpec{now: now, replay: driverCancelReplay(cancelID, "cancelled"), guard: guard, sql: driverCancelQueuedSQL,
		setParams: []any{expectedCancelGeneration + 1, cancelID, now, now}, fenceParams: []any{expectedCancelGeneration}, stale: "task changed during cancellation"})
}

// Persists a stop intent without claiming the remote run stopped
func DriverBeginTaskCancel(ctx context.Context, dbPath string, identity TaskIdentity, cancelIDValue any, expectedCancelGeneration int64, clock DriverClock) (map[string]any, error) {
	cancelID, err := driverIdentifier(cancelIDValue, "cancel_id")
	if err != nil {
		return nil, err
	}
	if err := driverCancelGeneration(expectedCancelGeneration); err != nil {
		return nil, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	guard := func(row *pyjson.Object) error {
		status := hrStr(row, "status")
		if DriverTerminalStatuses[status] || status == "queued" {
			return invalidTransition(fmt.Sprintf("cannot request remote stop in state '%s'", status))
		}
		return driverRequireCancelGeneration(row, expectedCancelGeneration)
	}
	return driverTransition(ctx, dbPath, identity, driverTransitionSpec{now: now, replay: driverCancelReplay(cancelID, "stopping"), guard: guard, sql: driverBeginStopSQL,
		setParams: []any{expectedCancelGeneration + 1, cancelID, now}, fenceParams: []any{expectedCancelGeneration}, stale: "task changed during stop request"})
}

// Commits cancellation only after the transport acknowledges the exact Stop
func DriverCompleteTaskCancel(ctx context.Context, dbPath string, identity TaskIdentity, cancelIDValue any, expectedCancelGeneration int64, clock DriverClock) (map[string]any, error) {
	cancelID, err := driverIdentifier(cancelIDValue, "cancel_id")
	if err != nil {
		return nil, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	guard := func(row *pyjson.Object) error {
		if hrStr(row, "status") != "stopping" || row.Vals["cancel_id"] == nil || hrStr(row, "cancel_id") != cancelID || hrInt(row, "cancel_generation") != expectedCancelGeneration {
			return staleTask("task stop acknowledgement is stale")
		}
		return nil
	}
	return driverTransition(ctx, dbPath, identity, driverTransitionSpec{now: now, replay: driverCancelReplay(cancelID, "cancelled"), guard: guard, sql: driverCompleteStopSQL,
		setParams: []any{now, now}, fenceParams: []any{cancelID, expectedCancelGeneration}, stale: "task changed during stop acknowledgement"})
}

// Fences abandoned running attempts without requeueing uncertain work, returning queued and indeterminate identities
func DriverRecoverRoom(ctx context.Context, dbPath string, lease DriverLease, clock DriverClock) (map[string][]TaskIdentity, error) {
	now, err := driverTimestamp(clock)
	if err != nil {
		return nil, err
	}
	foreignRunning := "room_id=? AND status='running' AND NOT (" + driverRunFence + ")"
	fence := append([]any{lease.RoomID}, driverRunFenceArgs(lease)...)
	out := map[string][]TaskIdentity{}
	err = driverTransaction(ctx, dbPath, func(conn state.SQLiteConn) error {
		if _, err := driverRequireActiveLease(ctx, conn, lease, now); err != nil {
			return err
		}
		stale, err := state.QueryObjects(ctx, conn, "SELECT * FROM hosted_room_driver_tasks WHERE "+foreignRunning+" "+driverTaskOrder, fence...)
		if err != nil {
			return err
		}
		if len(stale) > 0 {
			if _, err := conn.ExecContext(ctx, "UPDATE hosted_room_driver_tasks SET status='indeterminate', indeterminate_at=?, updated_at=?\n                    WHERE "+foreignRunning,
				append([]any{now, now}, fence...)...); err != nil {
				return err
			}
		}
		if err := captureTransitionLocked(ctx, conn, lease.RoomID); err != nil {
			return err
		}
		for _, status := range []string{"queued", "indeterminate"} {
			rows, err := driverTasksInOrder(ctx, conn, lease.RoomID, &status)
			if err != nil {
				return err
			}
			identities := make([]TaskIdentity, len(rows))
			for i, row := range rows {
				if identities[i], err = driverIdentityFromRow(row); err != nil {
					return err
				}
			}
			out[status] = identities
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Reads one task without mutating its state
func DriverGetTask(ctx context.Context, dbPath string, identity TaskIdentity) (map[string]any, error) {
	conn, err := driverConnect(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	row, err := driverLoadTask(ctx, conn, identity, true)
	if err != nil {
		return nil, err
	}
	return driverTaskFromRow(row, false)
}

// Room tasks in deterministic admission order, optionally of one status
func DriverListTasks(ctx context.Context, dbPath string, roomIDValue any, status *string) ([]map[string]any, error) {
	roomID, err := driverIdentifier(roomIDValue, "room_id")
	if err != nil {
		return nil, err
	}
	if status != nil && !DriverTaskStatuses[*status] {
		return nil, driverValidation("invalid task status")
	}
	conn, err := driverConnect(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := driverTasksInOrder(ctx, conn, roomID, status)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		if out[i], err = driverTaskFromRow(row, false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Bounds execution rows once their outcomes are durable in the room log
func DriverPrunePublishedTerminalTasks(ctx context.Context, dbPath string, roomIDValue any, clock DriverClock, retentionSeconds float64, retain any) (int64, error) {
	roomID, err := driverIdentifier(roomIDValue, "room_id")
	if err != nil {
		return 0, err
	}
	now, err := driverTimestamp(clock)
	if err != nil {
		return 0, err
	}
	if retentionSeconds <= 0 {
		return 0, driverValidation("retention_seconds must be positive")
	}
	retainCount, err := roomBoundedInt(retain, driverValidation, "retain must be a non-negative integer", 0, nil)
	if err != nil {
		return 0, err
	}
	var deleted int64
	err = driverTransaction(ctx, dbPath, func(conn state.SQLiteConn) error {
		publications, err := state.QueryRowObject(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='hosted_room_policy_publications'")
		if err != nil || publications == nil {
			return err
		}
		rows, err := state.QueryObjects(ctx, conn, `SELECT t.task_id, t.terminal_at FROM hosted_room_driver_tasks t
                WHERE t.room_id=? AND t.status IN ('settled', 'failed', 'cancelled')
                  AND EXISTS (SELECT 1 FROM hosted_room_policy_publications p
                              WHERE p.room_id=t.room_id AND p.task_id=t.task_id
                                AND p.kind IN ('turn.settled', 'turn.failed', 'turn.cancelled'))
                ORDER BY t.terminal_at DESC, t.task_id ASC`, roomID)
		if err != nil {
			return err
		}
		cutoff := now - retentionSeconds
		var candidates []any
		for index, row := range rows {
			if big.NewInt(int64(index)).Cmp(retainCount) >= 0 || (row.Vals["terminal_at"] != nil && hrFloat(row, "terminal_at") <= cutoff) {
				candidates = append(candidates, fmt.Sprint(row.Vals["task_id"]))
			}
			if len(candidates) == driverMaxTaskPruneBatch {
				break
			}
		}
		if len(candidates) == 0 {
			return nil
		}
		binds := strings.TrimSuffix(strings.Repeat("?,", len(candidates)), ",")
		count, err := roomExecCount(ctx, conn, "DELETE FROM hosted_room_driver_tasks WHERE room_id=? AND task_id IN ("+binds+")", append([]any{roomID}, candidates...)...)
		deleted = max(0, count)
		return err
	})
	return deleted, err
}

// Immutable admission of a turn including older payload versions, nil when none
func DriverGetTaskForTurn(ctx context.Context, dbPath string, identity TaskIdentity) (map[string]any, error) {
	conn, err := driverConnect(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	row, err := state.QueryRowObject(ctx, conn, `SELECT * FROM hosted_room_driver_tasks
               WHERE room_id=? AND thread_id=? AND turn_id=?`, identity.RoomID, identity.ThreadID, identity.TurnID)
	if err != nil || row == nil {
		return nil, err
	}
	return driverTaskFromRow(row, false)
}
