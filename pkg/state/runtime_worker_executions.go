package state

import (
	"context"
	"crypto/subtle"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Keyword arguments of register_worker_execution
type WorkerRegistration struct {
	Epoch          int64
	ExecutionID    string
	SessionID      string
	Generation     int64
	Kind           string
	AdoptionSecret string
	RequireIdle    bool
}

// Worker row without its adoption digest
func workerPublic(row *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range row.Keys {
		if k == "adoption_digest" {
			continue
		}
		v, _ := row.Get(k)
		out.Set(k, v)
	}
	return out
}

// Worker row assigned to the session at its current generation, refusing a missing, foreign or stale one
func workerAssignment(ctx context.Context, conn SQLiteConn, executionID, sessionID string, generation int64) (*pyjson.Object, error) {
	rows, err := queryObjects(ctx, conn, "SELECT * FROM worker_executions WHERE execution_id=?", executionID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, runtimeRefusal("not_found")
	}
	row := rows[0]
	if !pyjson.Equal(rowValue(row, "session_id"), sessionID) {
		return nil, runtimeRefusal("permission_denied")
	}
	if !pyjson.Equal(rowValue(row, "generation"), generation) {
		return nil, runtimeRefusal("stale_generation")
	}
	session, err := runtimeSession(ctx, conn, sessionID)
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(session["runtime_generation"], generation) {
		return nil, runtimeRefusal("stale_generation")
	}
	return row, nil
}

// The single admission of this generation and owner epoch targeting or descending through the session, nil when none
func linkedWorkerAdmission(ctx context.Context, conn SQLiteConn, sessionID string, generation int64, ownerEpoch any, statuses []string) (*pyjson.Object, error) {
	args := []any{generation, ownerEpoch}
	for _, s := range statuses {
		args = append(args, s)
	}
	args = append(args, sessionID, sessionID)
	rows, err := queryObjects(ctx, conn, `SELECT * FROM session_admissions
        WHERE generation=? AND owner_epoch=? AND status IN (`+placeholders(len(statuses))+`)
          AND (target_session_id=? OR EXISTS (
              SELECT 1 FROM json_each(session_admissions.lineage_json) WHERE value=?
          )) ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) > 1 {
		return nil, runtimeRefusal("admission_conflict")
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// True when any of the sessions holds an admission in one of the statuses
func anyAdmissionIn(ctx context.Context, conn SQLiteConn, sessionIDs map[string]bool, statusClause string) (bool, error) {
	for sid := range sessionIDs {
		row, err := connRow(ctx, conn, "SELECT 1 FROM session_admissions WHERE target_session_id=? AND status "+statusClause, sid)
		if err != nil {
			return false, err
		}
		if row != nil {
			return true, nil
		}
	}
	return false, nil
}

// Registers a worker for the session generation once, replaying an identical registration and refusing a busy lineage
func (d *SessionDB) RegisterWorkerExecution(ctx context.Context, in WorkerRegistration) (*pyjson.Object, error) {
	for _, v := range []string{in.ExecutionID, in.SessionID} {
		if err := runtimeText(v); err != nil {
			return nil, err
		}
	}
	switch in.Kind {
	case "cron", "child", "compute", "kanban":
	default:
		return nil, runtimeRefusal("invalid_params")
	}
	digest, err := secretDigest(in.AdoptionSecret)
	if err != nil {
		return nil, err
	}
	var out *pyjson.Object
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, in.Epoch); err != nil {
			return err
		}
		session, err := runtimeSession(ctx, conn, in.SessionID)
		if err != nil {
			return err
		}
		if !pyjson.Equal(session["runtime_generation"], in.Generation) {
			return runtimeRefusal("stale_generation")
		}
		olds, err := queryObjects(ctx, conn, "SELECT * FROM worker_executions WHERE execution_id=?", in.ExecutionID)
		if err != nil {
			return err
		}
		if len(olds) > 0 {
			old := olds[0]
			if !pyjson.Equal(rowValue(old, "session_id"), in.SessionID) || !pyjson.Equal(rowValue(old, "generation"), in.Generation) ||
				!pyjson.Equal(rowValue(old, "kind"), in.Kind) || !pyjson.Equal(rowValue(old, "owner_epoch"), in.Epoch) ||
				!pyjson.Equal(rowValue(old, "adoption_digest"), digest) {
				return runtimeRefusal("admission_conflict")
			}
			out = workerPublic(old)
			return nil
		}
		targets, err := runtimeLineage(ctx, conn, in.SessionID)
		if err != nil {
			return err
		}
		if in.RequireIdle {
			busy, err := anyAdmissionIn(ctx, conn, targets, "IN ('queued','started','unknown')")
			if err != nil {
				return err
			}
			if busy {
				return runtimeRefusal("stale_generation")
			}
		}
		states, err := workerStates(ctx, conn, in.SessionID)
		if err != nil {
			return err
		}
		if len(states) > 0 {
			return runtimeRefusal("stale_generation")
		}
		unknown, err := anyAdmissionIn(ctx, conn, targets, "='unknown'")
		if err != nil {
			return err
		}
		if unknown {
			return runtimeRefusal("unknown_execution")
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO worker_executions(execution_id,session_id,kind,owner_epoch,generation,status,adoption_digest)
            VALUES(?,?,?,?,?,'registered',?)`, in.ExecutionID, in.SessionID, in.Kind, in.Epoch, in.Generation, digest); err != nil {
			return err
		}
		row, err := workerAssignment(ctx, conn, in.ExecutionID, in.SessionID, in.Generation)
		if err != nil {
			return err
		}
		out = workerPublic(row)
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Moves a proven worker and its linked admission under this epoch, refusing a terminal, foreign secret or late handshake
func (d *SessionDB) AdoptWorkerExecution(ctx context.Context, epoch int64, executionID, sessionID string, generation int64, adoptionSecret string) (*pyjson.Object, error) {
	digest, err := secretDigest(adoptionSecret)
	if err != nil {
		return nil, err
	}
	var out *pyjson.Object
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		row, err := workerAssignment(ctx, conn, executionID, sessionID, generation)
		if err != nil {
			return err
		}
		if pyjson.Equal(rowValue(row, "status"), "terminal") {
			return runtimeRefusal("stale_generation")
		}
		stored, ok := rowValue(row, "adoption_digest").(string)
		if !ok {
			return &pyConvError{"TypeError", "unsupported operand types(s) or combination of types: 'NoneType' and 'str'"}
		}
		if subtle.ConstantTimeCompare([]byte(stored), []byte(digest)) != 1 {
			return runtimeRefusal("permission_denied")
		}
		ownerEpoch := rowValue(row, "owner_epoch")
		if strings.HasPrefix(executionID, "admission-worker:") && !pyjson.Equal(ownerEpoch, epoch) {
			return runtimeRefusal("stale_epoch")
		}
		linked, err := linkedWorkerAdmission(ctx, conn, sessionID, generation, ownerEpoch, []string{"started", "unknown"})
		if err != nil {
			return err
		}
		if linked != nil {
			res, err := conn.ExecContext(ctx, `UPDATE session_admissions SET owner_epoch=?,status='started'
                WHERE admission_id=? AND generation=? AND owner_epoch=?
                AND status IN ('started','unknown')`, epoch, rowValue(linked, "admission_id"), generation, ownerEpoch)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return runtimeRefusal("stale_generation")
			}
		}
		res, err := conn.ExecContext(ctx, "UPDATE worker_executions SET owner_epoch=?,status='running' WHERE execution_id=?", epoch, executionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return runtimeRefusal("stale_generation")
		}
		current, err := workerAssignment(ctx, conn, executionID, sessionID, generation)
		if err != nil {
			return err
		}
		out = workerPublic(current)
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return out, nil
}
