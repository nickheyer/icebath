package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Queued inputs one session holds at most
const AdmissionQueueMax = 32

// Outcome of a queued admission handed to its queue's running claim and not yet settled
const OutcomeSteering = "steering"

// Meta key prefix binding a replacement's identity to the admission it retired
const replacementPrefix = "gateway.queued_replacement.v1."

// One admitted session input as callers see it
type Admission struct {
	Seq             int64
	AdmissionID     string
	RequestID       string
	PrincipalID     string
	TargetSessionID string
	// Frozen input payload as its raw JSON object
	Payload    json.RawMessage
	Intent     string
	Status     string
	Outcome    sql.Null[string]
	OwnerEpoch sql.Null[int64]
	Generation sql.Null[int64]
	// Highest message id at the claim, set only on the row a claim returns
	TranscriptMark sql.Null[int64]
}

// One session_admissions row with the columns callers never see
type admissionRow struct {
	Admission
	lineageJSON   string
	payloadJSON   string
	payloadDigest string
	// Read from a retired tombstone rather than the live table
	retired bool
}

// Columns of an admission read
const admissionColumns = "seq, admission_id, request_id, principal_id, target_session_id, lineage_json, payload_json, payload_digest, " +
	"intent, status, outcome, owner_epoch, generation"

// Scans one admission row
func scanAdmission(scan func(dest ...any) error) (*admissionRow, error) {
	var r admissionRow
	var outcome sql.NullString
	var epoch, generation sql.NullInt64
	if err := scan(&r.Seq, &r.AdmissionID, &r.RequestID, &r.PrincipalID, &r.TargetSessionID, &r.lineageJSON, &r.payloadJSON, &r.payloadDigest,
		&r.Intent, &r.Status, &outcome, &epoch, &generation); err != nil {
		return nil, err
	}
	r.Payload = json.RawMessage(r.payloadJSON)
	r.Outcome = sql.Null[string]{V: outcome.String, Valid: outcome.Valid}
	r.OwnerEpoch = sql.Null[int64]{V: epoch.Int64, Valid: epoch.Valid}
	r.Generation = sql.Null[int64]{V: generation.Int64, Valid: generation.Valid}
	return &r, nil
}

// First admission row of a query, nil when it returned none
func queryAdmission(ctx context.Context, c Conn, where string, args ...any) (*admissionRow, error) {
	r, err := scanAdmission(c.QueryRowContext(ctx, "SELECT "+admissionColumns+" FROM session_admissions WHERE "+where, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// Every admission row of a query
func queryAdmissions(ctx context.Context, c Conn, where string, args ...any) ([]*admissionRow, error) {
	rows, err := c.QueryContext(ctx, "SELECT "+admissionColumns+" FROM session_admissions WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*admissionRow
	for rows.Next() {
		r, err := scanAdmission(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Admission a tombstone object describes
func admissionRowOfMembers(members []configMember) *admissionRow {
	text := func(key string) string {
		v, _ := memberValue(members, key)
		return jsonString(v)
	}
	number := func(key string) sql.Null[int64] {
		v, _ := memberValue(members, key)
		n, ok := jsonInt(v)
		return sql.Null[int64]{V: n, Valid: ok}
	}
	r := &admissionRow{retired: true, lineageJSON: text("lineage_json"), payloadJSON: text("payload_json"), payloadDigest: text("payload_digest")}
	r.Seq = number("seq").V
	r.AdmissionID, r.RequestID, r.PrincipalID = text("admission_id"), text("request_id"), text("principal_id")
	r.TargetSessionID, r.Intent, r.Status = text("target_session_id"), text("intent"), text("status")
	if v, ok := memberValue(members, "outcome"); ok && jsonIsString(v) {
		r.Outcome = sql.Null[string]{V: jsonString(v), Valid: true}
	}
	r.OwnerEpoch, r.Generation = number("owner_epoch"), number("generation")
	r.Payload = json.RawMessage(r.payloadJSON)
	return r
}

// Public admission of a tombstone object
func admissionOfMembers(members []configMember) Admission {
	return admissionRowOfMembers(members).Admission
}

// Live admission of an id or its retired tombstone, a not found refusal when neither exists
func admissionByID(ctx context.Context, c Conn, admissionID string) (*admissionRow, error) {
	row, err := queryAdmission(ctx, c, "admission_id=?", admissionID)
	if err != nil || row != nil {
		return row, err
	}
	members, err := terminalAdmission(ctx, c, admissionID)
	if err != nil {
		return nil, err
	}
	if members == nil {
		return nil, runtimeErr(ReasonNotFound)
	}
	return admissionRowOfMembers(members), nil
}

// Last id of a lineage list, the fallback when the list is empty or unreadable
func lineageTip(lineageJSON, fallback string) string {
	var lineage []string
	if json.Unmarshal([]byte(lineageJSON), &lineage) != nil || len(lineage) == 0 {
		return fallback
	}
	return lineage[len(lineage)-1]
}

// Retires the worker rows an admission's generation owns on its logical owner and its current physical transcript
func retireAdmissionWorkers(ctx context.Context, c Conn, row *admissionRow, ownerEpoch sql.Null[int64]) error {
	targets := []string{row.TargetSessionID}
	if tip := lineageTip(row.lineageJSON, row.TargetSessionID); tip != row.TargetSessionID {
		targets = append(targets, tip)
	}
	scope := "session_id IN (" + bindMarks(len(targets)) + ") AND generation=? AND owner_epoch=?"
	args := append(stringArgs(targets), nullInt(row.Generation), nullInt(ownerEpoch))
	live, err := queryStrings(ctx, c, "SELECT execution_id FROM worker_executions WHERE "+scope+" AND status!='terminal'", args...)
	if err != nil {
		return err
	}
	for _, executionID := range live {
		if err := compactTerminalReceipts(ctx, c, executionID); err != nil {
			return err
		}
	}
	_, err = c.ExecContext(ctx, "UPDATE worker_executions SET status='terminal' WHERE "+scope, args...)
	return err
}

// Bound nullable integer, NULL for an absent value
func nullInt(v sql.Null[int64]) any {
	if !v.Valid {
		return nil
	}
	return v.V
}

// How an input is admitted and which owner checks ride its transaction
type AdmitOptions struct {
	Epoch       int64
	PrincipalID string
	SessionID   string
	RequestID   string
	// Frozen input as one JSON object
	Payload json.RawMessage
	// queue, steer or redirect, queue when empty
	Intent string
	// Owner gate of a new write, run inside the transaction and free to refuse it
	AuthorizeWrite func(ctx context.Context, c Conn) error
	// Owner check of an exact live replay, run inside the transaction
	ReplayGuard func(ctx context.Context, c Conn) error
}

// Admits an input once per principal, target and request id, an exact retry returning the admission already made
func (s *Store) AdmitSessionInput(ctx context.Context, o AdmitOptions) (Admission, error) {
	if err := checkText(o.PrincipalID, o.SessionID, o.RequestID); err != nil {
		return Admission{}, err
	}
	intent := o.Intent
	if intent == "" {
		intent = "queue"
	}
	if intent != "queue" && intent != "steer" && intent != "redirect" {
		return Admission{}, runtimeErr(ReasonInvalidParams)
	}
	encoded, err := canonicalObject(o.Payload)
	if err != nil {
		return Admission{}, err
	}
	retired, err := s.RetryTerminalAdmission(ctx, o.Epoch, o.PrincipalID, o.SessionID, o.RequestID, o.Payload, intent)
	if err != nil {
		return Admission{}, err
	}
	if retired != nil {
		return *retired, nil
	}
	digest, err := AdmissionFingerprint(o.SessionID, map[string]any{"input": json.RawMessage(encoded), "intent": intent})
	if err != nil {
		return Admission{}, err
	}
	lineage, err := asciiJSONList([]string{o.SessionID})
	if err != nil {
		return Admission{}, err
	}
	var out Admission
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, o.Epoch); err != nil {
			return err
		}
		if _, _, err := runtimeCounters(ctx, c, o.SessionID); err != nil {
			return err
		}
		old, err := queryAdmission(ctx, c, "principal_id=? AND target_session_id=? AND request_id=?", o.PrincipalID, o.SessionID, o.RequestID)
		if err != nil {
			return err
		}
		if old != nil {
			if old.payloadDigest != digest {
				return admittedRequestConflict()
			}
			if o.ReplayGuard != nil {
				if err := o.ReplayGuard(ctx, c); err != nil {
					return err
				}
			}
			out = old.Admission
			return nil
		}
		var queued int
		if err := c.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_admissions WHERE target_session_id=?
            AND status='queued' `, o.SessionID).Scan(&queued); err != nil {
			return err
		}
		if queued >= AdmissionQueueMax {
			return runtimeErr(ReasonQueueFull)
		}
		if o.AuthorizeWrite != nil {
			if err := o.AuthorizeWrite(ctx, c); err != nil {
				return err
			}
		}
		admissionID, err := mintUID()
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, `INSERT INTO session_admissions(admission_id,request_id,principal_id,
            target_session_id,lineage_json,payload_json,payload_digest,intent,status,owner_epoch)
            VALUES(?,?,?,?,?,?,?,?,'queued',?)`, admissionID, o.RequestID, o.PrincipalID, o.SessionID, lineage, encoded, digest, intent, o.Epoch); err != nil {
			return err
		}
		row, err := admissionByID(ctx, c, admissionID)
		if err != nil {
			return err
		}
		out = row.Admission
		return nil
	})
	return out, err
}

// JSON list text of ids in the spaced ASCII layout
func asciiJSONList(ids []string) (string, error) {
	if ids == nil {
		ids = []string{}
	}
	return patchValueJSON(ids)
}

// Admission of an id, live or retired, false when neither exists
func (s *Store) GetSessionAdmission(ctx context.Context, admissionID string) (Admission, bool, error) {
	var out Admission
	found := false
	err := s.Read(ctx, func(c Conn) error {
		row, err := admissionByID(ctx, c, admissionID)
		if IsRuntimeReason(err, ReasonNotFound) {
			found = false
			return nil
		}
		if err != nil {
			return err
		}
		out, found = row.Admission, true
		return nil
	})
	return out, found, err
}

// Live admission at its exact durable identity, false when none exists
func (s *Store) FindSessionAdmission(ctx context.Context, principalID, sessionID, requestID string) (Admission, bool, error) {
	var out Admission
	found := false
	err := s.Read(ctx, func(c Conn) error {
		row, err := queryAdmission(ctx, c, "principal_id=? AND target_session_id=? AND request_id=?", principalID, sessionID, requestID)
		if err != nil || row == nil {
			found = false
			return err
		}
		out, found = row.Admission, true
		return nil
	})
	return out, found, err
}

// Admissions of a session in queue order, only the pending ones unless all are asked for
func (s *Store) ListSessionAdmissions(ctx context.Context, sessionID string, pendingOnly bool) ([]Admission, error) {
	where := "target_session_id=? "
	if pendingOnly {
		where += "AND status IN ('queued','started','unknown') "
	}
	var out []Admission
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		rows, err := queryAdmissions(ctx, c, where+"ORDER BY seq", sessionID)
		for _, r := range rows {
			out = append(out, r.Admission)
		}
		return err
	})
	return out, err
}

// What a claim of the queue head found
type ClaimOutcome int

// Nothing claimable, the head changed since the caller validated it, or a claimed admission
const (
	ClaimNone ClaimOutcome = iota
	ClaimHeadChanged
	ClaimStarted
)

// How the queue head is claimed
type ClaimOptions struct {
	Epoch     int64
	SessionID string
	// Owner admission gate run inside the transaction and free to refuse the claim
	Guard func(ctx context.Context, c Conn) error
	// Pins the head the caller validated, nil meaning it saw nothing queued, ignored unless PinHead is set
	Head    *string
	PinHead bool
}

// Claims the queue head as the session's next generation, nothing while started or worker bound work is in flight
func (s *Store) ClaimSessionInput(ctx context.Context, o ClaimOptions) (Admission, ClaimOutcome, error) {
	var out Admission
	outcome := ClaimNone
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		out, outcome = Admission{}, ClaimNone
		if err := checkEpoch(ctx, c, o.Epoch); err != nil {
			return err
		}
		if o.Guard != nil {
			if err := o.Guard(ctx, c); err != nil {
				return err
			}
		}
		_, generation, err := runtimeCounters(ctx, c, o.SessionID)
		if err != nil {
			return err
		}
		blocked, err := queryStrings(ctx, c, "SELECT status FROM session_admissions WHERE target_session_id=? AND status IN ('started','unknown')", o.SessionID)
		if err != nil {
			return err
		}
		if containsString(blocked, "unknown") {
			return runtimeErr(ReasonUnknownExecution)
		}
		workers, err := workerStates(ctx, c, o.SessionID)
		if err != nil {
			return err
		}
		if containsString(workers, "unknown") {
			return runtimeErr(ReasonUnknownExecution)
		}
		if len(blocked) > 0 || len(workers) > 0 {
			return nil
		}
		row, err := queryAdmission(ctx, c, "target_session_id=? AND status='queued' ORDER BY seq LIMIT 1", o.SessionID)
		if err != nil {
			return err
		}
		if o.PinHead {
			head := ""
			if row != nil {
				head = row.AdmissionID
			}
			if (o.Head == nil) != (row == nil) || (o.Head != nil && *o.Head != head) {
				outcome = ClaimHeadChanged
				return nil
			}
		}
		if row == nil {
			return nil
		}
		generation++
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_generation=? WHERE id=?", generation, o.SessionID); err != nil {
			return err
		}
		res, err := c.ExecContext(ctx, "UPDATE session_admissions SET status='started',owner_epoch=?,generation=? WHERE admission_id=? AND status='queued'",
			o.Epoch, generation, row.AdmissionID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return runtimeErr(ReasonStaleGeneration)
		}
		claimed, err := admissionByID(ctx, c, row.AdmissionID)
		if err != nil {
			return err
		}
		out = claimed.Admission
		var mark int64
		if err := c.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM messages").Scan(&mark); err != nil {
			return err
		}
		out.TranscriptMark = sql.Null[int64]{V: mark, Valid: true}
		outcome = ClaimStarted
		return nil
	})
	return out, outcome, err
}

// How a started admission settles
type SettleOptions struct {
	Epoch       int64
	AdmissionID string
	Generation  int64
	// completed, interrupted, rejected or failed
	Outcome string
	// Turn result stored for exact retries, nil storing none
	Result json.RawMessage
	// Steer transfers bound to this claim, each true when its text reached the claim's agent
	Transfers map[string]bool
	// Owner effect of the same transaction, free to refuse the settlement
	TerminalWrite func(ctx context.Context, c Conn, a Admission) error
}

// Text member of an admission payload, the raw JSON value
func payloadText(payloadJSON string) json.RawMessage {
	members, _ := objectMembers(json.RawMessage(payloadJSON))
	v, _ := memberValue(members, "text")
	return v
}

// Stores the compacted, encoded result of an admission under its result key
func storeAdmissionResult(ctx context.Context, c Conn, admissionID string, result json.RawMessage, payloadJSON string) error {
	members, ok := objectMembers(result)
	if !ok {
		return runtimeErr(ReasonInvalidParams)
	}
	encoded, err := canonicalObject(rawObject(encodeResult(compactResult(members, payloadText(payloadJSON)))))
	if err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?) "+
		"ON CONFLICT(key) DO UPDATE SET value=excluded.value", resultPrefix+admissionID, encoded)
	return err
}

// Settles a started admission as terminal with its outcome, retiring its workers and settling the steers bound to it
func (s *Store) SettleSessionInput(ctx context.Context, o SettleOptions) (Admission, error) {
	switch o.Outcome {
	case "completed", "interrupted", "rejected", "failed":
	default:
		return Admission{}, runtimeErr(ReasonInvalidParams)
	}
	var out Admission
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, o.Epoch); err != nil {
			return err
		}
		row, err := admissionByID(ctx, c, o.AdmissionID)
		if err != nil {
			return err
		}
		_, generation, err := runtimeCounters(ctx, c, row.TargetSessionID)
		if err != nil {
			return err
		}
		if row.Status != "started" || !row.OwnerEpoch.Valid || row.OwnerEpoch.V != o.Epoch ||
			!row.Generation.Valid || row.Generation.V != o.Generation || generation != o.Generation {
			return runtimeErr(ReasonStaleGeneration)
		}
		if err := retireAdmissionWorkers(ctx, c, row, sql.Null[int64]{V: o.Epoch, Valid: true}); err != nil {
			return err
		}
		if o.TerminalWrite != nil {
			if err := o.TerminalWrite(ctx, c, row.Admission); err != nil {
				return err
			}
		}
		if o.Result != nil {
			if err := storeAdmissionResult(ctx, c, o.AdmissionID, o.Result, row.payloadJSON); err != nil {
				return err
			}
		}
		if _, err := c.ExecContext(ctx, "UPDATE session_admissions SET status='terminal',outcome=? WHERE admission_id=?", o.Outcome, o.AdmissionID); err != nil {
			return err
		}
		if err := settleSteerTransfers(ctx, c, o.Epoch, o.Generation, o.Transfers, o.Outcome); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", row.TargetSessionID); err != nil {
			return err
		}
		settled, err := admissionByID(ctx, c, o.AdmissionID)
		if err != nil {
			return err
		}
		out = settled.Admission
		return nil
	})
	return out, err
}

// Statement cancelling a still queued admission
const cancelQueuedSQL = "UPDATE session_admissions SET status='terminal',outcome='cancelled' WHERE admission_id=?"

// Cancels a still queued admission, refusing one that started, is uncertain or was handed to a running claim
func (s *Store) CancelSessionInput(ctx context.Context, epoch int64, admissionID string) (Admission, error) {
	var out Admission
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		row, err := admissionByID(ctx, c, admissionID)
		if err != nil {
			return err
		}
		if row.Status == "unknown" {
			return runtimeErr(ReasonUnknownExecution)
		}
		if row.Status == "started" || row.Outcome.V == OutcomeSteering {
			return runtimeErr(ReasonStaleGeneration)
		}
		if row.Status == "queued" {
			if _, err := c.ExecContext(ctx, cancelQueuedSQL, admissionID); err != nil {
				return err
			}
		}
		if row, err = admissionByID(ctx, c, admissionID); err != nil {
			return err
		}
		out = row.Admission
		return nil
	})
	return out, err
}

// Reports an admission that is a committed steer transfer, in flight or settled as steered
func SteerReplayed(a Admission) bool {
	return a.Status == "terminal" && (a.Outcome.V == OutcomeSteering || a.Outcome.V == "steered")
}

// Puts a transfer still bound to the claim back in its queue place, true when one row moved
func requeueTransfer(ctx context.Context, c Conn, admissionID string, epoch, generation int64) (bool, error) {
	res, err := c.ExecContext(ctx, "UPDATE session_admissions SET status='queued',outcome=NULL,generation=NULL "+
		"WHERE admission_id=? AND status='terminal' AND outcome=? AND owner_epoch=? "+
		"AND generation=?", admissionID, OutcomeSteering, epoch, generation)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Hands a still queued admission to its queue's running claim, true when this call committed the transfer
func (s *Store) SteerSessionInput(ctx context.Context, epoch int64, admissionID string, generation int64) (Admission, bool, error) {
	var out Admission
	committed := false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		committed = false
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		row, err := admissionByID(ctx, c, admissionID)
		if err != nil {
			return err
		}
		if SteerReplayed(row.Admission) {
			out = row.Admission
			return nil
		}
		if row.Status != "queued" {
			return runtimeErr(ReasonAdmissionStarted)
		}
		var one int
		err = c.QueryRowContext(ctx, "SELECT 1 FROM session_admissions WHERE target_session_id=? AND status='started' "+
			"AND owner_epoch=? AND generation=?", row.TargetSessionID, epoch, generation).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return runtimeErr(ReasonSessionIdle)
		}
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE session_admissions SET status='terminal',outcome=?,owner_epoch=?,generation=? "+
			"WHERE admission_id=?", OutcomeSteering, epoch, generation, admissionID); err != nil {
			return err
		}
		if row, err = admissionByID(ctx, c, admissionID); err != nil {
			return err
		}
		out, committed = row.Admission, true
		return nil
	})
	return out, committed, err
}

// Settles each steer bound to a claim in the claim's terminal write, steered only when delivered and the claim completed
func settleSteerTransfers(ctx context.Context, c Conn, epoch, generation int64, transfers map[string]bool, outcome string) error {
	for _, id := range sortedKeys(transfers) {
		if !transfers[id] {
			if _, err := requeueTransfer(ctx, c, id, epoch, generation); err != nil {
				return err
			}
			continue
		}
		settled := "interrupted"
		if outcome == "completed" {
			settled = "steered"
		}
		if _, err := c.ExecContext(ctx, "UPDATE session_admissions SET outcome=? WHERE admission_id=? AND status='terminal' "+
			"AND outcome=? AND owner_epoch=? AND generation=?", settled, id, OutcomeSteering, epoch, generation); err != nil {
			return err
		}
	}
	return nil
}

// Queues again a transfer whose text never reached the claim's agent, false when the claim's settlement already decided it
func (s *Store) RevertSteerInput(ctx context.Context, epoch int64, admissionID string, generation int64) (bool, error) {
	moved := false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		var err error
		moved, err = requeueTransfer(ctx, c, admissionID, epoch, generation)
		return err
	})
	return moved, err
}

// Durable key binding a replacement's identity to the admission it retired
func ReplacementBinding(principalID, sessionID, requestID string) (string, error) {
	digest, err := AdmissionFingerprint(sessionID, map[string]string{"principal": principalID, "request": requestID})
	return replacementPrefix + digest, err
}

// Cancels the replaced, still queued admission inside the write that admits its successor and binds the two
func RetireReplacedInputIn(ctx context.Context, c Conn, admissionID, binding string) error {
	row, err := admissionByID(ctx, c, admissionID)
	if err != nil {
		return err
	}
	if row.Status != "queued" {
		return runtimeErr(ReasonAdmissionStarted)
	}
	if _, err := c.ExecContext(ctx, cancelQueuedSQL, admissionID); err != nil {
		return err
	}
	encoded, err := asciiJSONString(admissionID)
	if err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "INSERT OR REPLACE INTO state_meta(key,value) VALUES(?,?)", binding, encoded)
	return err
}

// Refuses the exact replay of a replacement unless its binding names the admission it retired
func CheckReplacedInputIn(ctx context.Context, c Conn, admissionID, binding string) error {
	saved, ok, err := metaValue(ctx, c, binding)
	if err != nil {
		return err
	}
	if !ok || !jsonIsString(json.RawMessage(saved)) || jsonString(json.RawMessage(saved)) != admissionID {
		return runtimeErr(ReasonAdmissionConflict)
	}
	return nil
}

// Moves the old owner's still queued admissions of a principal prefix onto a new session, returning the moved ids in queue order
func (s *Store) RehomeQueuedInputs(ctx context.Context, epoch int64, oldSessionID, newSessionID, principalPrefix string) ([]string, error) {
	if err := checkText(oldSessionID, newSessionID, principalPrefix); err != nil {
		return nil, err
	}
	lineage, err := asciiJSONList([]string{newSessionID})
	if err != nil {
		return nil, err
	}
	var moved []string
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		moved = nil
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		if _, _, err := runtimeCounters(ctx, c, newSessionID); err != nil {
			return err
		}
		unknown, err := queryStrings(ctx, c, "SELECT 1 FROM session_admissions WHERE target_session_id=? AND status='unknown'", oldSessionID)
		if err != nil || len(unknown) > 0 {
			return err
		}
		rows, err := queryAdmissions(ctx, c, "target_session_id=? AND status='queued' "+
			"AND substr(principal_id,1,?)=? ORDER BY seq", oldSessionID, len([]rune(principalPrefix)), principalPrefix)
		if err != nil {
			return err
		}
		for _, row := range rows {
			digest, err := AdmissionFingerprint(newSessionID, map[string]any{"input": json.RawMessage(row.payloadJSON), "intent": row.Intent})
			if err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, "UPDATE session_admissions SET target_session_id=?,lineage_json=?,payload_digest=? "+
				"WHERE admission_id=? AND status='queued'", newSessionID, lineage, digest, row.AdmissionID); err != nil {
				return err
			}
			moved = append(moved, row.AdmissionID)
		}
		if len(rows) > 0 {
			for _, sid := range []string{oldSessionID, newSessionID} {
				if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", sid); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return moved, err
}

// Points a local owner's still queued automation rows that name a pre-reset transcript at a new target, returning the retargeted ids
func (s *Store) RetargetLocalAutomation(ctx context.Context, epoch int64, sessionID, principalID string, oldTargets []string, newTarget string) ([]string, error) {
	if err := checkText(append([]string{sessionID, principalID, newTarget}, oldTargets...)...); err != nil {
		return nil, err
	}
	var moved []string
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		moved = nil
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		if _, _, err := runtimeCounters(ctx, c, newTarget); err != nil {
			return err
		}
		unknown, err := queryStrings(ctx, c, "SELECT 1 FROM session_admissions WHERE target_session_id=? AND status='unknown'", sessionID)
		if err != nil || len(unknown) > 0 {
			return err
		}
		rows, err := queryAdmissions(ctx, c, "target_session_id=? AND status='queued' AND principal_id=? ORDER BY seq", sessionID, principalID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			payload, ok := objectMembers(json.RawMessage(row.payloadJSON))
			if !ok {
				continue
			}
			rawDescriptor, _ := memberValue(payload, "local_automation_v1")
			descriptor, isObject := objectMembers(rawDescriptor)
			target, _ := memberValue(descriptor, "target")
			if !isObject || !jsonIsString(target) || !containsString(oldTargets, jsonString(target)) {
				continue
			}
			encodedTarget, err := json.Marshal(newTarget)
			if err != nil {
				return err
			}
			updated := rawObject(withMember(payload, "local_automation_v1", rawObject(withMember(descriptor, "target", encodedTarget))))
			encoded, err := canonicalObject(updated)
			if err != nil {
				return err
			}
			digest, err := AdmissionFingerprint(sessionID, map[string]any{"input": updated, "intent": row.Intent})
			if err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, "UPDATE session_admissions SET payload_json=?,payload_digest=? WHERE admission_id=? AND status='queued'",
				encoded, digest, row.AdmissionID); err != nil {
				return err
			}
			moved = append(moved, row.AdmissionID)
		}
		if len(moved) > 0 {
			_, err = c.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", sessionID)
		}
		return err
	})
	return moved, err
}

// Marks work started under an earlier owner epoch as uncertain, never replaying it, returning the admissions marked
func (s *Store) RecoverSessionInputs(ctx context.Context, epoch int64) (int64, error) {
	var marked int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		if err := retireStaleChildren(ctx, c, epoch); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE worker_executions SET status='unknown' WHERE status IN ('registered','running') AND owner_epoch!=?", epoch); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE session_admissions SET outcome='interrupted' WHERE status='terminal' AND outcome=? "+
			"AND owner_epoch!=?", OutcomeSteering, epoch); err != nil {
			return err
		}
		res, err := c.ExecContext(ctx, "UPDATE session_admissions SET status='unknown' WHERE status='started' AND owner_epoch!=?", epoch)
		if err != nil {
			return err
		}
		marked, err = res.RowsAffected()
		return err
	})
	return marked, err
}

// How an uncertain admission is resolved
type ResolveUnknownOptions struct {
	Epoch       int64
	AdmissionID string
	Generation  int64
	// Owner effect of the same transaction, free to refuse the resolution
	TerminalWrite func(ctx context.Context, c Conn, a Admission) error
	// Outcome and result this owner captured before its settlement write failed, empty outcome when it captured none
	CapturedOutcome string
	CapturedResult  json.RawMessage
}

// Resolves an uncertain admission as terminal by explicit operator acknowledgement, never requeueing it
func (s *Store) ResolveUnknownSessionInput(ctx context.Context, o ResolveUnknownOptions) (Admission, error) {
	var out Admission
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, o.Epoch); err != nil {
			return err
		}
		row, err := admissionByID(ctx, c, o.AdmissionID)
		if err != nil {
			return err
		}
		sameGeneration := row.Generation.Valid && row.Generation.V == o.Generation
		if sameGeneration && row.Status == "terminal" {
			stored, err := metaExists(ctx, c, resultPrefix+o.AdmissionID)
			if err != nil {
				return err
			}
			if row.Outcome.V == "interrupted" || stored {
				out = row.Admission
				return nil
			}
		}
		if row.Status != "unknown" || !sameGeneration {
			return runtimeErr(ReasonStaleGeneration)
		}
		outcome := "interrupted"
		if o.CapturedOutcome != "" && row.OwnerEpoch.Valid && row.OwnerEpoch.V == o.Epoch {
			outcome = o.CapturedOutcome
			if outcome != "completed" && outcome != "interrupted" && outcome != "failed" {
				return runtimeErr(ReasonInvalidParams)
			}
			if err := storeAdmissionResult(ctx, c, o.AdmissionID, o.CapturedResult, row.payloadJSON); err != nil {
				return err
			}
		}
		if o.TerminalWrite != nil {
			if err := o.TerminalWrite(ctx, c, row.Admission); err != nil {
				return err
			}
		}
		if err := retireAdmissionWorkers(ctx, c, row, row.OwnerEpoch); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE session_admissions SET status='terminal',outcome=? WHERE admission_id=?", outcome, o.AdmissionID); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", row.TargetSessionID); err != nil {
			return err
		}
		if row, err = admissionByID(ctx, c, o.AdmissionID); err != nil {
			return err
		}
		out = row.Admission
		return nil
	})
	return out, err
}

// Meta key and digest of a mutation request's durable exact retry receipt
func mutationReceiptKey(principalID, sessionID, requestID, operation string, payload json.RawMessage, expectedRevision int64, expectedGeneration *int64) (string, string, error) {
	identity, err := AdmissionFingerprint(sessionID, map[string]string{"principal": principalID, "request": requestID})
	if err != nil {
		return "", "", err
	}
	fingerprint := map[string]any{"operation": operation, "payload": payload, "expected_revision": expectedRevision}
	if expectedGeneration != nil {
		fingerprint["expected_generation"] = *expectedGeneration
	}
	digest, err := AdmissionFingerprint(sessionID, fingerprint)
	return "gateway.mutation.v1." + identity, digest, err
}

// Stored result of an exact mutation retry, nil when unseen and a conflict for a different request under the same key
func replayMutationReceipt(ctx context.Context, c Conn, key, digest string) (json.RawMessage, error) {
	old, ok, err := metaValue(ctx, c, key)
	if err != nil || !ok {
		return nil, err
	}
	receipt, isObject := objectMembers(json.RawMessage(old))
	stored, _ := memberValue(receipt, "digest")
	if !isObject || jsonString(stored) != digest {
		return nil, runtimeErr(ReasonAdmissionConflict)
	}
	result, _ := memberValue(receipt, "result")
	return result, nil
}

// Reports whether a principal's request on a session already holds a mutation receipt
func (s *Store) HasMutationReceipt(ctx context.Context, principalID, sessionID, requestID string) (bool, error) {
	if err := checkText(principalID, sessionID, requestID); err != nil {
		return false, err
	}
	key, _, err := mutationReceiptKey(principalID, sessionID, requestID, "model", json.RawMessage("{}"), 0, nil)
	if err != nil {
		return false, err
	}
	found := false
	err = s.Read(ctx, func(c Conn) error {
		var err error
		found, err = metaExists(ctx, c, key)
		return err
	})
	return found, err
}
