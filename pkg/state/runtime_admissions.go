package state

import (
	"context"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	// Queued inputs one session holds, the busy queue cap of the gateway runner
	AdmissionQueueMax = 32
	// Outcome of a queued admission handed to its FIFO's running claim and not yet settled
	Steering = "steering"
)

// Field and detail of the one admission conflict proving the request id was already admitted
var (
	AdmittedRequestConflictField  = "request_id"
	AdmittedRequestConflictDetail = "this request id was already admitted with another payload"
)

// Admission conflict naming the request id already admitted with another payload
func admittedRequestConflict() error {
	return NewRuntimeStoreError("admission_conflict", &AdmittedRequestConflictField, &AdmittedRequestConflictDetail)
}

// Refuses an empty runtime identifier or one longer than 1024 characters
func runtimeText(value string) error {
	if value == "" || utf8.RuneCountInString(value) > 1024 {
		return runtimeRefusal("invalid_params")
	}
	return nil
}

// Refuses a writer whose epoch is not the current runtime epoch
func runtimeEpoch(ctx context.Context, conn SQLiteConn, epoch int64) error {
	row, err := connRow(ctx, conn, "SELECT epoch FROM runtime_epoch WHERE singleton=1")
	if err != nil {
		return err
	}
	if current, ok := exactInt(rowAt(row, "epoch")); row == nil || !ok || current != epoch {
		return runtimeRefusal("stale_epoch")
	}
	return nil
}

// Column of a plain row, nil for a missing row
func rowAt(row map[string]any, key string) any {
	if row == nil {
		return nil
	}
	return row[key]
}

// Sessions row of the id, refused not found when absent
func runtimeSession(ctx context.Context, conn SQLiteConn, sessionID string) (map[string]any, error) {
	row, err := connRow(ctx, conn, "SELECT * FROM sessions WHERE id=?", sessionID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, runtimeRefusal("not_found")
	}
	return row, nil
}

// Live admission row else its retired tombstone, refused not found when neither exists
func runtimeAdmission(ctx context.Context, conn SQLiteConn, admissionID string) (*pyjson.Object, error) {
	rows, err := queryObjects(ctx, conn, "SELECT * FROM session_admissions WHERE admission_id=?", admissionID)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		return rows[0], nil
	}
	row, err := terminalAdmission(ctx, conn, admissionID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, runtimeRefusal("not_found")
	}
	return row, nil
}

// Retires the workers the admission's generation owns on its logical owner and its current physical transcript
func retireAdmissionWorkers(ctx context.Context, conn SQLiteConn, row *pyjson.Object, ownerEpoch any) error {
	lineageText, _ := rowValue(row, "lineage_json").(string)
	decoded, err := pyjson.Loads(lineageText)
	if err != nil {
		return pyValueError(err.Error())
	}
	target := rowValue(row, "target_session_id")
	last := target
	if lineage, ok := decoded.([]any); ok && len(lineage) > 0 {
		last = lineage[len(lineage)-1]
	}
	targets := []any{target}
	if !pyjson.Equal(last, target) {
		targets = append(targets, last)
	}
	scope := "session_id IN (" + placeholders(len(targets)) + ") AND generation=? AND owner_epoch=?"
	args := append(append([]any{}, targets...), rowValue(row, "generation"), ownerEpoch)
	live, err := queryStrings(ctx, conn, "SELECT execution_id FROM worker_executions WHERE "+scope+" AND status!='terminal'", args...)
	if err != nil {
		return err
	}
	for _, executionID := range live {
		if err := compactTerminalReceipts(ctx, conn, executionID); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, "UPDATE worker_executions SET status='terminal' WHERE "+scope, args...)
	return err
}

// Public admission dict with its payload decoded and its digest, lineage and retention stamp dropped
func admissionRow(row *pyjson.Object) (*pyjson.Object, error) {
	if row == nil {
		return nil, nil
	}
	result := row.Copy()
	raw, has := result.Pop("payload_json")
	if !has {
		return nil, &stateTypedError{"KeyError", "payload_json"}
	}
	text, ok := raw.(string)
	if !ok {
		return nil, &stateTypedError{"TypeError", "the JSON object must be str"}
	}
	payload, err := pyjson.Loads(text)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	result.Set("payload", payload)
	for _, k := range []string{"payload_digest", "lineage_json"} {
		if _, has := result.Pop(k); !has {
			return nil, &stateTypedError{"KeyError", k}
		}
	}
	result.Pop("retired_at")
	return result, nil
}

// Advances the runtime epoch for a new owner, which must hold the profile ownership lock
func (d *SessionDB) BeginRuntimeEpoch(ctx context.Context, instanceID string) (int64, error) {
	if err := runtimeText(instanceID); err != nil {
		return 0, err
	}
	var epoch int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO runtime_epoch(singleton,epoch,instance_id) VALUES(1,1,?)
            ON CONFLICT(singleton) DO UPDATE SET epoch=runtime_epoch.epoch+1, instance_id=excluded.instance_id`, instanceID); err != nil {
			return err
		}
		row, err := connRow(ctx, conn, "SELECT epoch FROM runtime_epoch WHERE singleton=1")
		if err != nil {
			return err
		}
		epoch, _ = exactInt(rowAt(row, "epoch"))
		return nil
	}, nil)
	return epoch, err
}

// One input to admit, the intent defaulting to queue and the guards running inside the owning write
type SessionInput struct {
	Epoch       int64
	PrincipalID string
	SessionID   string
	RequestID   string
	Payload     *pyjson.Object
	Intent      string
	// Raises to refuse a new write, never seeing exact or terminal replays
	AuthorizeWrite func(ctx context.Context, conn SQLiteConn) error
	// Runs on the exact live replay inside the same transaction
	ReplayGuard func(ctx context.Context, conn SQLiteConn) error
}

// Queue, steer or redirect, empty meaning queue
func (in SessionInput) intent() string {
	if in.Intent == "" {
		return "queue"
	}
	return in.Intent
}

// Canonical encoding of the admission payload, refused invalid params when it is not JSON safe
func encodeAdmissionPayload(payload *pyjson.Object) (string, error) {
	if payload == nil {
		return "", runtimeRefusal("invalid_params")
	}
	return runtimeJSON(payload)
}

// Fingerprint of the target with the decoded payload and intent
func admissionDigest(in SessionInput) (string, error) {
	encoded, err := encodeAdmissionPayload(in.Payload)
	if err != nil {
		return "", err
	}
	input, err := pyjson.Loads(encoded)
	if err != nil {
		return "", pyValueError(err.Error())
	}
	return AdmissionFingerprint(in.SessionID, map[string]any{"input": input, "intent": in.intent()})
}

// Admits input exactly once per principal request, replaying an identical retry and refusing a changed one
func (d *SessionDB) AdmitSessionInput(ctx context.Context, in SessionInput) (*pyjson.Object, error) {
	for _, v := range []string{in.PrincipalID, in.SessionID, in.RequestID} {
		if err := runtimeText(v); err != nil {
			return nil, err
		}
	}
	intent := in.intent()
	if intent != "queue" && intent != "steer" && intent != "redirect" {
		return nil, runtimeRefusal("invalid_params")
	}
	encoded, err := encodeAdmissionPayload(in.Payload)
	if err != nil {
		return nil, err
	}
	retired, err := d.retryTerminalAdmission(ctx, in)
	if err != nil || retired != nil {
		return retired, err
	}
	digest, err := admissionDigest(in)
	if err != nil {
		return nil, err
	}
	lineage, err := pyjson.Dumps([]any{in.SessionID})
	if err != nil {
		return nil, err
	}
	var out *pyjson.Object
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, in.Epoch); err != nil {
			return err
		}
		if _, err := runtimeSession(ctx, conn, in.SessionID); err != nil {
			return err
		}
		old, err := queryObjects(ctx, conn, `SELECT * FROM session_admissions
            WHERE principal_id=? AND target_session_id=? AND request_id=?`, in.PrincipalID, in.SessionID, in.RequestID)
		if err != nil {
			return err
		}
		if len(old) > 0 {
			if !pyjson.Equal(rowValue(old[0], "payload_digest"), digest) {
				return admittedRequestConflict()
			}
			if in.ReplayGuard != nil {
				if err := in.ReplayGuard(ctx, conn); err != nil {
					return err
				}
			}
			out, err = admissionRow(old[0])
			return err
		}
		counted, err := connRow(ctx, conn, `SELECT COUNT(*) AS n FROM session_admissions WHERE target_session_id=?
            AND status='queued' `, in.SessionID)
		if err != nil {
			return err
		}
		if queued, _ := exactInt(rowAt(counted, "n")); queued >= AdmissionQueueMax {
			return runtimeRefusal("queue_full")
		}
		if in.AuthorizeWrite != nil {
			if err := in.AuthorizeWrite(ctx, conn); err != nil {
				return err
			}
		}
		admissionID := uuid4Hex()
		if _, err := conn.ExecContext(ctx, `INSERT INTO session_admissions(admission_id,request_id,principal_id,
            target_session_id,lineage_json,payload_json,payload_digest,intent,status,owner_epoch)
            VALUES(?,?,?,?,?,?,?,?,'queued',?)`,
			admissionID, in.RequestID, in.PrincipalID, in.SessionID, lineage, encoded, digest, intent, in.Epoch); err != nil {
			return err
		}
		row, err := runtimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		out, err = admissionRow(row)
		return err
	}, nil)
	return out, err
}

// Admission by id, its retired tombstone when the live row is gone, nil when neither exists
func (d *SessionDB) GetSessionAdmission(ctx context.Context, admissionID string) (*pyjson.Object, error) {
	var out *pyjson.Object
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		rows, err := queryObjects(ctx, conn, "SELECT * FROM session_admissions WHERE admission_id=?", admissionID)
		if err != nil {
			return err
		}
		var row *pyjson.Object
		if len(rows) > 0 {
			row = rows[0]
		} else if row, err = terminalAdmission(ctx, conn, admissionID); err != nil {
			return err
		}
		out, err = admissionRow(row)
		return err
	})
	return out, err
}

// Admission at its exact durable identity, the unique principal, target and request key
func (d *SessionDB) FindSessionAdmission(ctx context.Context, principalID, sessionID, requestID string) (*pyjson.Object, error) {
	var out *pyjson.Object
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		rows, err := queryObjects(ctx, conn, "SELECT * FROM session_admissions WHERE principal_id=? AND target_session_id=? "+
			"AND request_id=?", principalID, sessionID, requestID)
		if err != nil || len(rows) == 0 {
			return err
		}
		out, err = admissionRow(rows[0])
		return err
	})
	return out, err
}

// Admissions of a session in FIFO order, only queued, started and unknown ones when pending only
func (d *SessionDB) ListSessionAdmissions(ctx context.Context, sessionID string, pendingOnly bool) ([]*pyjson.Object, error) {
	query := "SELECT * FROM session_admissions WHERE target_session_id=? "
	if pendingOnly {
		query += "AND status IN ('queued','started','unknown') "
	}
	query += "ORDER BY seq"
	out := []*pyjson.Object{}
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		rows, err := queryObjects(ctx, conn, query, sessionID)
		if err != nil {
			return err
		}
		out = out[:0]
		for _, row := range rows {
			public, err := admissionRow(row)
			if err != nil {
				return err
			}
			out = append(out, public)
		}
		return nil
	})
	return out, err
}

// FIFO head claim, the guard refusing inside the write and a set head check refusing a moved head
type ClaimInput struct {
	Epoch     int64
	SessionID string
	Guard     func(ctx context.Context, conn SQLiteConn) error
	// Head is the admission id the caller validated, nil meaning it saw nothing queued
	CheckHead bool
	Head      *string
}

// Claims the FIFO head, nil when nothing is claimable and moved when the head is no longer the one checked
func (d *SessionDB) ClaimSessionInput(ctx context.Context, in ClaimInput) (claimed *pyjson.Object, moved bool, err error) {
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		claimed, moved = nil, false
		if err := runtimeEpoch(ctx, conn, in.Epoch); err != nil {
			return err
		}
		if in.Guard != nil {
			if err := in.Guard(ctx, conn); err != nil {
				return err
			}
		}
		session, err := runtimeSession(ctx, conn, in.SessionID)
		if err != nil {
			return err
		}
		blocked, err := queryStrings(ctx, conn, "SELECT status FROM session_admissions WHERE target_session_id=? AND status IN ('started','unknown')", in.SessionID)
		if err != nil {
			return err
		}
		for _, status := range blocked {
			if status == "unknown" {
				return runtimeRefusal("unknown_execution")
			}
		}
		workers, err := workerStates(ctx, conn, in.SessionID)
		if err != nil {
			return err
		}
		if workers["unknown"] {
			return runtimeRefusal("unknown_execution")
		}
		if len(blocked) > 0 || len(workers) > 0 {
			return nil
		}
		heads, err := queryObjects(ctx, conn, "SELECT * FROM session_admissions WHERE target_session_id=? AND status='queued' ORDER BY seq LIMIT 1", in.SessionID)
		if err != nil {
			return err
		}
		if in.CheckHead {
			var seen any
			if len(heads) > 0 {
				seen = rowValue(heads[0], "admission_id")
			}
			var want any
			if in.Head != nil {
				want = *in.Head
			}
			if !pyjson.Equal(seen, want) {
				moved = true
				return nil
			}
		}
		if len(heads) == 0 {
			return nil
		}
		current, ok := exactInt(session["runtime_generation"])
		if !ok {
			return &stateTypedError{"TypeError", "unsupported operand type(s) for +"}
		}
		generation := current + 1
		admissionID, _ := rowValue(heads[0], "admission_id").(string)
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_generation=? WHERE id=?", generation, in.SessionID); err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='started',owner_epoch=?,generation=? WHERE admission_id=? AND status='queued'", in.Epoch, generation, admissionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return runtimeRefusal("stale_generation")
		}
		row, err := runtimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		if claimed, err = admissionRow(row); err != nil {
			return err
		}
		mark, err := connRow(ctx, conn, "SELECT COALESCE(MAX(id), 0) AS mark FROM messages")
		if err != nil {
			return err
		}
		claimed.Set("transcript_mark", rowAt(mark, "mark"))
		return nil
	}, nil)
	if err != nil {
		return nil, false, err
	}
	return claimed, moved, nil
}

// Text field of a decoded admission payload, nil when absent
func admissionPayloadText(row *pyjson.Object) (any, error) {
	text, _ := rowValue(row, "payload_json").(string)
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, &stateTypedError{"AttributeError", "payload has no attribute get"}
	}
	return rowValue(payload, "text"), nil
}

// Stores the compacted and encoded turn result under the admission's result key
func storeAdmissionResult(ctx context.Context, conn SQLiteConn, row *pyjson.Object, admissionID string, result *pyjson.Object) error {
	userMessage, err := admissionPayloadText(row)
	if err != nil {
		return err
	}
	compacted, err := CompactResult(result, userMessage)
	if err != nil {
		return err
	}
	encoded, err := runtimeJSON(encodeTerminalResult(compacted))
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?) "+
		"ON CONFLICT(key) DO UPDATE SET value=excluded.value", TerminalResultPrefix+admissionID, encoded)
	return err
}

// Settlement of a started claim, the terminal write running in the same transaction
type SettleInput struct {
	Epoch       int64
	AdmissionID string
	Generation  int64
	Outcome     string
	Result      *pyjson.Object
	// Steer transfers bound to this claim, true when the text reached the claim's agent
	Transfers     map[string]bool
	TerminalWrite func(ctx context.Context, conn SQLiteConn, row *pyjson.Object) error
}

// Settles a started claim at its own epoch and generation, retiring its workers and storing its result
func (d *SessionDB) SettleSessionInput(ctx context.Context, in SettleInput) (*pyjson.Object, error) {
	switch in.Outcome {
	case "completed", "interrupted", "rejected", "failed":
	default:
		return nil, runtimeRefusal("invalid_params")
	}
	var out *pyjson.Object
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, in.Epoch); err != nil {
			return err
		}
		row, err := runtimeAdmission(ctx, conn, in.AdmissionID)
		if err != nil {
			return err
		}
		target, _ := rowValue(row, "target_session_id").(string)
		session, err := runtimeSession(ctx, conn, target)
		if err != nil {
			return err
		}
		owner, ownerOK := exactInt(rowValue(row, "owner_epoch"))
		generation, genOK := exactInt(rowValue(row, "generation"))
		current, curOK := exactInt(session["runtime_generation"])
		if !pyjson.Equal(rowValue(row, "status"), "started") || !ownerOK || owner != in.Epoch ||
			!genOK || generation != in.Generation || !curOK || current != in.Generation {
			return runtimeRefusal("stale_generation")
		}
		if err := retireAdmissionWorkers(ctx, conn, row, in.Epoch); err != nil {
			return err
		}
		if in.TerminalWrite != nil {
			if err := in.TerminalWrite(ctx, conn, row); err != nil {
				return err
			}
		}
		if in.Result != nil {
			if err := storeAdmissionResult(ctx, conn, row, in.AdmissionID, in.Result); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='terminal',outcome=? WHERE admission_id=?", in.Outcome, in.AdmissionID); err != nil {
			return err
		}
		if err := SettleSteerTransfers(ctx, conn, in.Epoch, in.Generation, in.Transfers, in.Outcome); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", target); err != nil {
			return err
		}
		settled, err := runtimeAdmission(ctx, conn, in.AdmissionID)
		if err != nil {
			return err
		}
		out, err = admissionRow(settled)
		return err
	}, nil)
	return out, err
}

// Cancels a queued admission, refusing a started, unknown or steering one and replaying a terminal one
func (d *SessionDB) CancelSessionInput(ctx context.Context, epoch int64, admissionID string) (*pyjson.Object, error) {
	var out *pyjson.Object
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		row, err := runtimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		status := rowValue(row, "status")
		if pyjson.Equal(status, "unknown") {
			return runtimeRefusal("unknown_execution")
		}
		if pyjson.Equal(status, "started") || pyjson.Equal(rowValue(row, "outcome"), Steering) {
			return runtimeRefusal("stale_generation")
		}
		if pyjson.Equal(status, "queued") {
			if _, err := conn.ExecContext(ctx, cancelQueuedAdmissionQ, admissionID); err != nil {
				return err
			}
		}
		current, err := runtimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		out, err = admissionRow(current)
		return err
	}, nil)
	return out, err
}

// True for a committed steer transfer an exact retry replays
func SteerReplayed(row *pyjson.Object) bool {
	outcome := rowValue(row, "outcome")
	return pyjson.Equal(rowValue(row, "status"), "terminal") && (pyjson.Equal(outcome, Steering) || pyjson.Equal(outcome, "steered"))
}

// Puts a transfer still bound to the claim back in its FIFO place
func requeueTransfer(ctx context.Context, conn SQLiteConn, admissionID string, epoch, generation int64) (bool, error) {
	res, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='queued',outcome=NULL,generation=NULL "+
		"WHERE admission_id=? AND status='terminal' AND outcome=? AND owner_epoch=? "+
		"AND generation=?", admissionID, Steering, epoch, generation)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Hands a queued admission to the running claim of the generation, true when this call committed the transfer
func (d *SessionDB) SteerSessionInput(ctx context.Context, epoch int64, admissionID string, generation int64) (*pyjson.Object, bool, error) {
	var out *pyjson.Object
	committed := false
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		committed = false
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		row, err := runtimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		if SteerReplayed(row) {
			out, err = admissionRow(row)
			return err
		}
		if !pyjson.Equal(rowValue(row, "status"), "queued") {
			return runtimeRefusal("admission_started")
		}
		running, err := connRow(ctx, conn, "SELECT 1 AS one FROM session_admissions WHERE target_session_id=? AND status='started' "+
			"AND owner_epoch=? AND generation=?", rowValue(row, "target_session_id"), epoch, generation)
		if err != nil {
			return err
		}
		if running == nil {
			return runtimeRefusal("session_idle")
		}
		if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='terminal',outcome=?,owner_epoch=?,generation=? "+
			"WHERE admission_id=?", Steering, epoch, generation, admissionID); err != nil {
			return err
		}
		current, err := runtimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		out, err = admissionRow(current)
		committed = err == nil
		return err
	}, nil)
	if err != nil {
		return nil, false, err
	}
	return out, committed, nil
}

// Settles each transfer bound to the claim, steered when delivered and completed, interrupted when delivered otherwise, queued again when undelivered
func SettleSteerTransfers(ctx context.Context, conn SQLiteConn, epoch, generation int64, transfers map[string]bool, outcome string) error {
	settled := "interrupted"
	if outcome == "completed" {
		settled = "steered"
	}
	for admissionID, delivered := range transfers {
		if !delivered {
			if _, err := requeueTransfer(ctx, conn, admissionID, epoch, generation); err != nil {
				return err
			}
			continue
		}
		if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET outcome=? WHERE admission_id=? AND status='terminal' "+
			"AND outcome=? AND owner_epoch=? AND generation=?", settled, admissionID, Steering, epoch, generation); err != nil {
			return err
		}
	}
	return nil
}

// Queues an undelivered transfer again in its original place, false when settlement already decided it
func (d *SessionDB) RevertSteerInput(ctx context.Context, epoch int64, admissionID string, generation int64) (bool, error) {
	requeued := false
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		var err error
		requeued, err = requeueTransfer(ctx, conn, admissionID, epoch, generation)
		return err
	}, nil)
	return requeued, err
}

// Marks started work of earlier epochs unknown and never replays it, returning how many claims became unknown
func (d *SessionDB) RecoverSessionInputs(ctx context.Context, epoch int64) (int64, error) {
	var recovered int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		if err := retireStaleChildren(ctx, conn, epoch); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE worker_executions SET status='unknown' WHERE status IN ('registered','running') AND owner_epoch!=?", epoch); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET outcome='interrupted' WHERE status='terminal' AND outcome=? "+
			"AND owner_epoch!=?", Steering, epoch); err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='unknown' WHERE status='started' AND owner_epoch!=?", epoch)
		if err != nil {
			return err
		}
		recovered, err = res.RowsAffected()
		return err
	}, nil)
	return recovered, err
}

// Result this owner captured for a turn before its settlement write failed
type CapturedResult struct {
	Outcome string
	Stored  *pyjson.Object
}

// Operator acknowledgement of an unknown turn, resolving it without requeue and committing a captured result of this epoch
func (d *SessionDB) ResolveUnknownSessionInput(ctx context.Context, epoch int64, admissionID string, generation int64,
	terminalWrite func(ctx context.Context, conn SQLiteConn, row *pyjson.Object) error, captured *CapturedResult) (*pyjson.Object, error) {
	var out *pyjson.Object
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		row, err := runtimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		rowGeneration, genOK := exactInt(rowValue(row, "generation"))
		sameGeneration := genOK && rowGeneration == generation
		status := rowValue(row, "status")
		if sameGeneration && pyjson.Equal(status, "terminal") {
			replay := pyjson.Equal(rowValue(row, "outcome"), "interrupted")
			if !replay {
				stored, err := connRow(ctx, conn, "SELECT 1 AS one FROM state_meta WHERE key=?", TerminalResultPrefix+admissionID)
				if err != nil {
					return err
				}
				replay = stored != nil
			}
			if replay {
				out, err = admissionRow(row)
				return err
			}
		}
		if !pyjson.Equal(status, "unknown") || !sameGeneration {
			return runtimeRefusal("stale_generation")
		}
		outcome := "interrupted"
		owner, ownerOK := exactInt(rowValue(row, "owner_epoch"))
		if captured != nil && ownerOK && owner == epoch {
			outcome = captured.Outcome
			if outcome != "completed" && outcome != "interrupted" && outcome != "failed" {
				return runtimeRefusal("invalid_params")
			}
			if captured.Stored == nil {
				return &stateTypedError{"TypeError", "'NoneType' object is not iterable"}
			}
			if err := storeAdmissionResult(ctx, conn, row, admissionID, captured.Stored); err != nil {
				return err
			}
		}
		if terminalWrite != nil {
			if err := terminalWrite(ctx, conn, row); err != nil {
				return err
			}
		}
		if err := retireAdmissionWorkers(ctx, conn, row, rowValue(row, "owner_epoch")); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='terminal',outcome=? WHERE admission_id=?", outcome, admissionID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", rowValue(row, "target_session_id")); err != nil {
			return err
		}
		current, err := runtimeAdmission(ctx, conn, admissionID)
		if err != nil {
			return err
		}
		out, err = admissionRow(current)
		return err
	}, nil)
	return out, err
}
