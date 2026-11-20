package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// The on_session_delete event a committed runtime mutation owes its plugins
type SessionGoneEvent struct {
	SessionIDs []string
	Reason     string
}

// Keyword arguments of mutate_runtime_session
type RuntimeMutation struct {
	Epoch              int64
	PrincipalID        string
	SessionID          string
	RequestID          string
	ExpectedRevision   int64
	Operation          string
	Payload            any
	ExpectedGeneration *int64
	// Raises to refuse the mutation of the targets it names before anything is written
	LiveGuard func(targets []string) error
	// Raises to refuse a new write, never seeing replays
	AuthorizeWrite func(ctx context.Context, conn SQLiteConn) error
	PrepareOnly    bool
	Prepared       *pyjson.Object
}

// Operations that fence the execution generation
var generationFencedOperations = map[string]bool{"delete": true, "rewind": true, "reset": true, "branch": true, "model": true, "compress": true}

// Commits a closed metadata edit and its retry receipt in one transaction, firing the delete event after
func (d *SessionDB) MutateRuntimeSession(ctx context.Context, in RuntimeMutation) (*pyjson.Object, error) {
	for _, v := range []string{in.PrincipalID, in.SessionID, in.RequestID} {
		if err := runtimeText(v); err != nil {
			return nil, err
		}
	}
	if in.ExpectedRevision < 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := ValidateAction(in.Operation, in.Payload); err != nil {
		return nil, err
	}
	if in.ExpectedGeneration != nil && *in.ExpectedGeneration < 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	if generationFencedOperations[in.Operation] && in.ExpectedGeneration == nil {
		return nil, runtimeRefusal("invalid_params")
	}
	encoded, err := runtimeJSON(in.Payload)
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(encoded)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	payload := decoded.(*pyjson.Object)
	key, digest, err := MutationReceiptKey(in.PrincipalID, in.SessionID, in.RequestID, in.Operation, payload, in.ExpectedRevision, in.ExpectedGeneration)
	if err != nil {
		return nil, err
	}
	var result *pyjson.Object
	var event *SessionGoneEvent
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		result, event = nil, nil
		if err := runtimeEpoch(ctx, conn, in.Epoch); err != nil {
			return err
		}
		replay, err := ReplayMutationReceipt(ctx, conn, key, digest)
		if err != nil {
			return err
		}
		if replay != nil {
			stored, ok := replay.(*pyjson.Object)
			if !ok {
				return &stateTypedError{"TypeError", "stored mutation result is not a dict"}
			}
			result = stored
			return nil
		}
		session, err := connRow(ctx, conn, "SELECT * FROM sessions WHERE id=?", in.SessionID)
		if err != nil {
			return err
		}
		if session == nil && in.Operation != "import" {
			return runtimeRefusal("not_found")
		}
		var revision any = int64(0)
		if session != nil {
			revision = session["runtime_revision"]
		}
		if !pyjson.Equal(revision, in.ExpectedRevision) {
			return runtimeRefusal("revision_conflict")
		}
		if in.ExpectedGeneration != nil && (session == nil || !pyjson.Equal(session["runtime_generation"], *in.ExpectedGeneration)) {
			return runtimeRefusal("stale_generation")
		}
		if in.LiveGuard != nil {
			targets := []string{in.SessionID}
			if in.Operation == "delete" {
				if targets, err = DeleteTargets(ctx, conn, in.SessionID); err != nil {
					return err
				}
			}
			if err := in.LiveGuard(targets); err != nil {
				return err
			}
		}
		if in.AuthorizeWrite != nil {
			if err := in.AuthorizeWrite(ctx, conn); err != nil {
				return err
			}
		}
		if in.PrepareOnly {
			snapshot, err := d.LocalSnapshot(ctx, conn, in.SessionID)
			if err != nil {
				return err
			}
			if in.Operation == "branch" {
				if err := d.RequireIdle(ctx, conn, distinctSessionIDs(in.SessionID, snapshotTarget(snapshot))); err != nil {
					return err
				}
			}
			result = pyjson.ObjectOf("snapshot", snapshot)
			return nil
		}
		affected, projection, err := d.applyAction(ctx, conn, in.SessionID, in.Operation, payload, in.Prepared)
		if err != nil {
			return err
		}
		if in.Operation == "delete" {
			ids := []string{in.SessionID}
			if deleted, _ := projection.Get("deleted_ids"); pyTruthy(deleted) {
				ids = []string{}
				for _, sid := range deleted.([]any) {
					ids = append(ids, pyStr(sid))
				}
			}
			event = &SessionGoneEvent{SessionIDs: ids, Reason: "delete"}
		} else if archived, _ := payload.Get("archived"); archived == true && session != nil && !pyTruthy(session["archived"]) {
			ids := affected
			if len(ids) == 0 {
				ids = []string{in.SessionID}
			}
			event = &SessionGoneEvent{SessionIDs: ids, Reason: "archive"}
		}
		for _, target := range affected {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", target); err != nil {
				return err
			}
		}
		updated, err := connRow(ctx, conn, "SELECT * FROM sessions WHERE id=?", in.SessionID)
		if err != nil {
			return err
		}
		var newRevision, newGeneration any
		if updated != nil {
			newRevision, newGeneration = updated["runtime_revision"], updated["runtime_generation"]
		} else {
			newRevision = in.ExpectedRevision + 1
			if in.ExpectedGeneration != nil {
				newGeneration = *in.ExpectedGeneration
			}
		}
		out := pyjson.ObjectOf("session_id", in.SessionID, "revision", newRevision, "execution_generation", newGeneration, "operation", in.Operation)
		out.Update(projection)
		receipt, err := runtimeJSON(pyjson.ObjectOf("digest", digest, "result", out))
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", key, receipt); err != nil {
			return err
		}
		result = out
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	if event != nil {
		d.EmitSessionDeleted(ctx, event.SessionIDs, event.Reason, in.SessionID, "")
	}
	return result, nil
}

// State key of the one time legacy prompt admissions import marker
const legacyImportKey = "gateway.prompt_admissions_import.v1"

// Compression chain of a session walked on the transaction connection, a cycle or runaway refused
func canonicalChain(ctx context.Context, conn SQLiteConn, sessionID string) ([]string, error) {
	if _, err := runtimeSession(ctx, conn, sessionID); err != nil {
		return nil, err
	}
	chain := []string{sessionID}
	for range 100 {
		child, err := connRow(ctx, conn, chainStepSQL, chain[len(chain)-1])
		if err != nil {
			return nil, err
		}
		if child == nil {
			return chain, nil
		}
		id := pyStr(child["id"])
		for _, seen := range chain {
			if seen == id {
				return nil, runtimeRefusal("admission_conflict")
			}
		}
		chain = append(chain, id)
	}
	return nil, runtimeRefusal("admission_conflict")
}

// Inserts one frozen legacy admission row against the canonical tip of its target
func importLegacyRow(ctx context.Context, conn SQLiteConn, row *pyjson.Object, epoch int64, principalID string) error {
	for _, field := range []string{"admission_id", "target_session_id", "root"} {
		if _, err := runtimeTextOf(rowValue(row, field)); err != nil {
			return err
		}
	}
	lineageRaw, _ := rowValue(row, "lineage").(string)
	lineageDecoded, err := pyjson.Loads(lineageRaw)
	if err != nil {
		return pyValueError(err.Error())
	}
	lineage, ok := lineageDecoded.([]any)
	targetID := pyStr(rowValue(row, "target_session_id"))
	inLineage := false
	for _, item := range lineage {
		if pyjson.Equal(item, targetID) {
			inLineage = true
		}
	}
	if !ok || !inLineage {
		return runtimeRefusal("invalid_params")
	}
	payloadRaw, _ := rowValue(row, "payload").(string)
	payloadDecoded, err := pyjson.Loads(payloadRaw)
	if err != nil {
		return pyValueError(err.Error())
	}
	encoded, err := runtimeJSON(payloadDecoded)
	if err != nil {
		return err
	}
	payload := payloadDecoded.(*pyjson.Object)
	intent, has := payload.Get("intent")
	if !has {
		intent = "queue"
	}
	if intent == "interrupt" {
		intent = "redirect"
	}
	status := pyStr(rowValue(row, "status"))
	switch intent {
	case "queue", "steer", "redirect":
	default:
		return runtimeRefusal("invalid_params")
	}
	switch status {
	case "queued", "started", "unknown", "terminal":
	default:
		return runtimeRefusal("invalid_params")
	}
	chain, err := canonicalChain(ctx, conn, targetID)
	if err != nil {
		return err
	}
	target := chain[len(chain)-1]
	if status == "started" {
		status = "unknown"
	}
	generation := rowValue(row, "generation")
	if generation == nil && status == "unknown" {
		session, err := runtimeSession(ctx, conn, target)
		if err != nil {
			return err
		}
		current, _ := exactInt(session["runtime_generation"])
		generation = current + 1
	}
	if generation != nil {
		if n, ok := exactInt(generation); !ok || n < 0 {
			return runtimeRefusal("invalid_params")
		}
	}
	digest, err := AdmissionFingerprint(target, map[string]any{"input": payload, "intent": intent})
	if err != nil {
		return err
	}
	chainJSON, err := pyjson.Dumps(anyStrings(chain))
	if err != nil {
		return err
	}
	admissionID := rowValue(row, "admission_id")
	if _, err := conn.ExecContext(ctx, `INSERT INTO session_admissions(admission_id,request_id,principal_id,
        target_session_id,lineage_json,payload_json,payload_digest,intent,status,outcome,owner_epoch,generation)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, admissionID, admissionID, principalID,
		target, chainJSON, encoded, digest, intent, status, rowValue(row, "outcome"), epoch, generation); err != nil {
		return err
	}
	if generation != nil {
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_generation=MAX(runtime_generation,?) WHERE id=?", generation, target); err != nil {
			return err
		}
	}
	return nil
}

// Imports a frozen legacy admissions snapshot once, the source file left as rollback evidence
func (d *SessionDB) ImportLegacySessionAdmissions(ctx context.Context, epoch int64, sourcePath, principalID string, writersDrained bool) (int64, error) {
	if !writersDrained {
		return 0, runtimeRefusal("invalid_params")
	}
	if err := runtimeText(principalID); err != nil {
		return 0, err
	}
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		return 0, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return 0, err
	}
	source, err := ConnectTracked(pathAsURI(resolved)+"?mode=ro", WithURI(), WithTrackingPath(resolved))
	if err != nil {
		return 0, err
	}
	defer source.Close()
	if _, err := source.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return 0, err
	}
	if _, err := source.ExecContext(ctx, "BEGIN"); err != nil {
		return 0, err
	}
	rows, err := queryObjects(ctx, source, "SELECT * FROM admissions ORDER BY seq")
	if err != nil {
		return 0, err
	}
	executions := []*pyjson.Object{}
	hasExecutions, err := connRow(ctx, source, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='executions'")
	if err != nil {
		return 0, err
	}
	if hasExecutions != nil {
		if executions, err = queryObjects(ctx, source, "SELECT * FROM executions ORDER BY root"); err != nil {
			return 0, err
		}
	}
	snapshot, err := runtimeJSON(pyjson.ObjectOf("rows", anyObjectList(rows), "executions", anyObjectList(executions)))
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256([]byte(snapshot))
	marker, err := runtimeJSON(pyjson.ObjectOf("source", resolved, "fingerprint", hex.EncodeToString(sum[:]),
		"imported_id_count", int64(len(rows)), "principal_id", principalID))
	if err != nil {
		return 0, err
	}
	var imported int64
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		imported = 0
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		old, has, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", legacyImportKey)
		if err != nil {
			return err
		}
		if has {
			if !pyjson.Equal(old, marker) {
				return runtimeRefusal("admission_conflict")
			}
			return nil
		}
		for _, execution := range executions {
			generation, ok := exactInt(rowValue(execution, "generation"))
			if !ok || generation < 0 {
				return runtimeRefusal("invalid_params")
			}
			root, err := runtimeTextOf(rowValue(execution, "root"))
			if err != nil {
				return err
			}
			chain, err := canonicalChain(ctx, conn, root)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_generation=MAX(runtime_generation,?) WHERE id=?", generation, chain[len(chain)-1]); err != nil {
				return err
			}
		}
		for _, row := range rows {
			if err := importLegacyRow(ctx, conn, row, epoch, principalID); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", legacyImportKey, marker); err != nil {
			return err
		}
		imported = int64(len(rows))
		return nil
	}, nil)
	return imported, err
}

// Row objects as a JSON list value
func anyObjectList(rows []*pyjson.Object) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}
