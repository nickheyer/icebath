package kanban

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const currentRunEventSQL = "SELECT e.payload FROM task_events e JOIN tasks t ON t.current_run_id=e.run_id " +
	"WHERE t.id=? AND e.task_id=t.id AND e.kind=? ORDER BY e.id DESC LIMIT 1"

// Parsed owner_admitted receipt of the current run, nil without one and empty when unreadable
func ownerReceipt(ctx context.Context, conn state.SQLiteConn, taskID string) (*pyjson.Object, error) {
	row, err := QueryOne(ctx, conn, currentRunEventSQL, taskID, "owner_admitted")
	if err != nil || row == nil {
		return nil, err
	}
	return jsonDict(row.Get("payload")), nil
}

// Status and outcome of the owner ledger admission a receipt names, ok false when nothing readable
func ownerAdmissionRow(ctx context.Context, receipt *pyjson.Object) (status, outcome any, ok bool) {
	if receipt == nil || receipt.Len() == 0 {
		return nil, nil, false
	}
	dbValue, hasDB := receipt.Get("db")
	sessionID, hasSession := receipt.Get("session_id")
	requestID, hasRequest := receipt.Get("request_id")
	dbPath, isStr := dbValue.(string)
	if !hasDB || !isStr || !hasSession || !hasRequest {
		return nil, nil, false
	}
	uri, err := pypath.AsURI(dbPath)
	if err != nil {
		return nil, nil, false
	}
	owner, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, false
	}
	defer owner.Close()
	err = owner.QueryRowContext(ctx, "SELECT status,outcome FROM session_admissions WHERE target_session_id=? AND request_id=?",
		sessionID, requestID).Scan(&status, &outcome)
	if err != nil {
		return nil, nil, false
	}
	return status, outcome, true
}

// True when a missing or ambiguous owner receipt must not authorize another attempt
func ownerReclaimPaused(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	receipt, err := ownerReceipt(ctx, conn, taskID)
	if err != nil || receipt == nil {
		return false, err
	}
	status, outcome, ok := ownerAdmissionRow(ctx, receipt)
	if ok && status == "terminal" {
		return outcome == "interrupted", nil
	}
	if !ok || status == "queued" || status == "unknown" {
		return true, nil
	}
	pidValue, hasPid := receipt.Get("pid")
	pid, isInt := pyint.Instance(pidValue)
	if !hasPid || !isInt || pid.Sign() < 0 || !pid.IsInt64() {
		return true, nil
	}
	birth, err := proc.WorkerBirth(int(pid.Int64()))
	if err != nil {
		return true, nil
	}
	recorded, hasBirth := receipt.Get("birth")
	return !hasBirth || !pyrepr.Equal(birth, recorded), nil
}

// Newest payload of a kind on the current run, empty when absent or unreadable
func latestRunEvent(ctx context.Context, conn state.SQLiteConn, taskID, kind string) (*pyjson.Object, error) {
	row, err := QueryOne(ctx, conn, currentRunEventSQL, taskID, kind)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return pyjson.NewObject(), nil
	}
	return jsonDict(row.Get("payload")), nil
}

// True when this host issued the lock, the only pids judged here
func hostLocalLock(lock any) bool {
	return truthy(lock) && strings.HasPrefix(pyrepr.Str(lock), hostPrefix())
}

// True when the owner bound an interpreter to the current run and that interpreter is dead
func boundInterpreterGone(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	bound, err := latestRunEvent(ctx, conn, taskID, "worker_bound")
	if err != nil || bound.Len() == 0 {
		return false, err
	}
	return !workerAlive(objGet(bound, "pid"), objGet(bound, "started_at")), nil
}

// True when the receipt pid is gone, a zombie, or now names a process of another birth
func ownerProcessGone(receipt *pyjson.Object) bool {
	pidValue, hasPid := receipt.Get("pid")
	if !hasPid {
		return false
	}
	n, err := pyint.Of(pidValue)
	if err != nil {
		return false
	}
	if !n.IsInt64() || !pidAlive(int(n.Int64())) {
		return true
	}
	current, err := proc.WorkerBirth(int(n.Int64()))
	if errors.Is(err, proc.ErrNoSuchProcess) {
		return true
	}
	if err != nil {
		return false
	}
	birth := objGet(receipt, "birth")
	return birth != nil && !pyrepr.Equal(current, birth)
}

type runWorkerPid struct {
	pid         int
	fingerprint any
}

// Every worker pid recorded for the current run with its spawn fingerprint
func runWorkerPids(ctx context.Context, conn state.SQLiteConn, taskRow *Row, taskID string) ([]runWorkerPid, error) {
	recorded := [][2]any{{taskRow.Get("worker_pid"), taskRow.Get("worker_started_at")}}
	for _, kind := range []string{"spawned", "worker_bound"} {
		p, err := latestRunEvent(ctx, conn, taskID, kind)
		if err != nil {
			return nil, err
		}
		recorded = append(recorded, [2]any{objGet(p, "pid"), objGet(p, "started_at")})
	}
	var out []runWorkerPid
	for _, r := range recorded {
		if !truthy(r[0]) {
			continue
		}
		n, err := pyint.Of(r[0])
		if err != nil {
			return nil, err
		}
		out = append(out, runWorkerPid{int(n.Int64()), r[1]})
	}
	return out, nil
}

// True when an unresolved owner receipt cannot still be executing this attempt
func ownerAttemptProvablyOver(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	bound, err := latestRunEvent(ctx, conn, taskID, "worker_bound")
	if err != nil || bound.Len() > 0 {
		return false, err
	}
	receipt, err := ownerReceipt(ctx, conn, taskID)
	if err != nil || receipt == nil {
		return false, err
	}
	row, err := QueryOne(ctx, conn, "SELECT claim_lock, claim_expires, worker_pid, worker_started_at, last_heartbeat_at FROM tasks WHERE id=?", taskID)
	if err != nil || row == nil || !hostLocalLock(row.Get("claim_lock")) {
		return false, err
	}
	pids, err := runWorkerPids(ctx, conn, row, taskID)
	if err != nil {
		return false, err
	}
	for _, p := range pids {
		if workerAlive(p.pid, p.fingerprint) {
			return false, nil
		}
	}
	status, _, ok := ownerAdmissionRow(ctx, receipt)
	settled := ok && status == "terminal"
	if settled && len(pids) > 0 {
		return true, nil
	}
	now := time.Now().Unix()
	expires := toEpoch(row.Get("claim_expires"))
	if expires == nil || *expires >= now {
		return false, nil
	}
	if hb := toEpoch(row.Get("last_heartbeat_at")); hb != nil && now-*hb <= DefaultClaimHeartbeatMaxStaleSeconds {
		return false, nil
	}
	return settled || (len(pids) > 0 && ownerProcessGone(receipt)), nil
}

// One clause naming why the owner says the attempt ended, empty when it recorded nothing
func ownerAttemptNote(ctx context.Context, conn state.SQLiteConn, taskID string, lost *pyjson.Object) (string, error) {
	var err error
	if lost == nil {
		if lost, err = latestRunEvent(ctx, conn, taskID, "worker_lost"); err != nil {
			return "", err
		}
	}
	if reason := objGet(lost, "reason"); truthy(reason) {
		r := []rune(pyrepr.Str(reason))
		if len(r) > 80 {
			r = r[:80]
		}
		return "managed worker lost: " + string(r), nil
	}
	receipt, err := ownerReceipt(ctx, conn, taskID)
	if err != nil || receipt == nil {
		return "", err
	}
	if status, outcome, ok := ownerAdmissionRow(ctx, receipt); ok && status == "terminal" {
		settledAs := "terminal"
		if truthy(outcome) {
			settledAs = pyrepr.Str(outcome)
		}
		return "owner admission settled " + settledAs, nil
	}
	return "", nil
}

// Run error of a stale lock reclaim, naming how the attempt ended when the owner recorded it
func reclaimError(ctx context.Context, conn state.SQLiteConn, taskID string, claimLock *string) (string, error) {
	note, err := ownerAttemptNote(ctx, conn, taskID, nil)
	if err != nil {
		return "", err
	}
	text := "stale_lock=" + pyStrOpt(claimLock)
	if note != "" {
		text += " [" + note + "]"
	}
	return text, nil
}

// Folds the owner record of how the attempt ended into one dead worker booking
func applyAttemptNote(ctx context.Context, conn state.SQLiteConn, taskID string, pid int, dead *deadWorker) error {
	lost, err := latestRunEvent(ctx, conn, taskID, "worker_lost")
	if err != nil {
		return err
	}
	note, err := ownerAttemptNote(ctx, conn, taskID, lost)
	if err != nil || note == "" {
		return err
	}
	dead.EventPayload.Set("attempt_note", note)
	if lost.Len() > 0 {
		dead.EventPayload.Set("worker_lost", lost)
		if dead.ProtocolViolation {
			dead.ProtocolViolation = false
			dead.EventPayload.Delete("protocol_violation")
			dead.EventPayload.Set("exit_kind", dead.Kind)
			dead.EventKind = "crashed"
			dead.ErrorText = fmt.Sprintf("pid %d exited cleanly after its managed worker was lost.", pid) +
				strings.TrimPrefix(dead.ErrorText, protocolViolationError)
		}
	}
	head, output, found := strings.Cut(dead.ErrorText, WorkerOutputSep)
	sep := ""
	if found {
		sep = WorkerOutputSep
	}
	dead.ErrorText = head + " [" + note + "]" + sep + output
	return nil
}

// Seam over the reclaim gate, replaced by tests the way hermes tests monkeypatch the module
var ownerReclaimBlocked = ownerReclaimBlockedLive

// True when a reclaim path must leave the task alone, a dead bound interpreter freeing only the crash sweep
func ownerReclaimBlockedLive(ctx context.Context, conn state.SQLiteConn, taskID string, crashSweep bool) (bool, error) {
	paused, err := ownerReclaimPaused(ctx, conn, taskID)
	if err != nil || !paused {
		return false, err
	}
	if crashSweep {
		gone, err := boundInterpreterGone(ctx, conn, taskID)
		if err != nil || gone {
			return false, err
		}
	}
	over, err := ownerAttemptProvablyOver(ctx, conn, taskID)
	return !over, err
}

// Under lock gate recheck after termination, a local signal letting a dead bound interpreter through
func ownerReclaimBlockedAfterKill(ctx context.Context, conn state.SQLiteConn, taskID string, signalled bool) (bool, error) {
	return ownerReclaimBlocked(ctx, conn, taskID, signalled)
}

// Exit code the managed run authority recorded for the current run under the same claim and pid
func managedRunExitCode(ctx context.Context, conn state.SQLiteConn, taskID string, workerPid, claimLock any) (any, error) {
	latest := func(kind string) (*pyjson.Object, error) {
		row, err := QueryOne(ctx, conn, currentRunEventSQL, taskID, kind)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return pyjson.NewObject(), nil
		}
		var text string
		switch raw := row.Get("payload").(type) {
		case string:
			text = raw
		case []byte:
			text = string(raw)
		default:
			return nil, fmt.Errorf("TypeError: the JSON object must be str, bytes or bytearray, not %s", pyrepr.TypeName(raw))
		}
		v, err := pyjson.Loads(text)
		if o, ok := v.(*pyjson.Object); ok && err == nil {
			return o, nil
		}
		return pyjson.NewObject(), nil
	}
	result, err := latest("worker_result")
	if err != nil {
		return nil, err
	}
	bound, err := latest("worker_bound")
	if err != nil {
		return nil, err
	}
	pids := []any{workerPid}
	if pyrepr.Equal(objGet(bound, "claim_lock"), claimLock) {
		pids = append(pids, objGet(bound, "pid"))
	}
	if !pyrepr.Equal(objGet(result, "claim_lock"), claimLock) {
		return nil, nil
	}
	for _, p := range pids {
		if pyrepr.Equal(objGet(result, "pid"), p) {
			return objGet(result, "exit_code"), nil
		}
	}
	return nil, nil
}
