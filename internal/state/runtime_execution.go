package state

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const legacyImportKey = "gateway.prompt_admissions_import.v1"

const workerPayloadLimit = 4 * 1024 * 1024

type WorkerExecution struct {
	ExecutionID  string
	SessionID    string
	Kind         string
	OwnerEpoch   int64
	Generation   int64
	Status       string
	LastSequence int64
}

type workerRow struct {
	WorkerExecution
	adoptionDigest string
}

type WorkerHandler func(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error)

type WorkerReceipt struct {
	Epoch       int64
	ExecutionID string
	SessionID   string
	Generation  int64
	Sequence    int64
	Operation   string
	Payload     json.RawMessage
}

func canonicalChainIn(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	if _, _, err := runtimeCounters(ctx, c, sessionID); err != nil {
		return nil, err
	}
	chain := []string{sessionID}
	for range 100 {
		child, err := chainStep(ctx, c, chain[len(chain)-1])
		if err != nil {
			return nil, err
		}
		if child == "" {
			return chain, nil
		}
		if containsString(chain, child) {
			return nil, runtimeErr(ReasonAdmissionConflict)
		}
		chain = append(chain, child)
	}
	return nil, runtimeErr(ReasonAdmissionConflict)
}

func legacyText(row map[string]any, field string) (string, error) {
	v, ok := row[field]
	if !ok {
		return "", fmt.Errorf("legacy admission row has no %s column", field)
	}
	text, isText := v.(string)
	if !isText {
		return "", runtimeErr(ReasonInvalidParams)
	}
	return text, checkText(text)
}

func legacyJSON(row map[string]any, field string) (json.RawMessage, error) {
	v, ok := row[field]
	if !ok {
		return nil, fmt.Errorf("legacy admission row has no %s column", field)
	}
	var text string
	switch t := v.(type) {
	case string:
		text = t
	case []byte:
		text = string(t)
	default:
		return nil, fmt.Errorf("legacy admission %s is not JSON text", field)
	}
	if !json.Valid([]byte(text)) {
		return nil, fmt.Errorf("legacy admission %s is not valid JSON", field)
	}
	return json.RawMessage(text), nil
}

func importLegacyRow(ctx context.Context, c Conn, row map[string]any, epoch int64, principalID string) error {
	for _, field := range []string{"admission_id", "target_session_id", "root"} {
		if _, err := legacyText(row, field); err != nil {
			return err
		}
	}
	admissionID, target := row["admission_id"].(string), row["target_session_id"].(string)
	lineageRaw, err := legacyJSON(row, "lineage")
	if err != nil {
		return err
	}
	lineage, ok := arrayElements(lineageRaw)
	listed := false
	for _, e := range lineage {
		listed = listed || (jsonIsString(e) && jsonString(e) == target)
	}
	if !ok || !listed {
		return runtimeErr(ReasonInvalidParams)
	}
	payloadRaw, err := legacyJSON(row, "payload")
	if err != nil {
		return err
	}
	encoded, err := canonicalObject(payloadRaw)
	if err != nil {
		return err
	}
	members, _ := objectMembers(payloadRaw)
	intent := "queue"
	if v, has := memberValue(members, "intent"); has {
		intent = "\x00"
		if jsonIsString(v) {
			intent = jsonString(v)
		}
	}
	if intent == "interrupt" {
		intent = "redirect"
	}
	status, _ := row["status"].(string)
	if (intent != "queue" && intent != "steer" && intent != "redirect") ||
		(status != "queued" && status != "started" && status != "unknown" && status != "terminal") {
		return runtimeErr(ReasonInvalidParams)
	}
	chain, err := canonicalChainIn(ctx, c, target)
	if err != nil {
		return err
	}
	target = chain[len(chain)-1]
	if status == "started" {
		status = "unknown"
	}
	var generation any
	switch g := row["generation"].(type) {
	case nil:
		if status == "unknown" {
			_, current, err := runtimeCounters(ctx, c, target)
			if err != nil {
				return err
			}
			generation = current + 1
		}
	case int64:
		if g < 0 {
			return runtimeErr(ReasonInvalidParams)
		}
		generation = g
	default:
		return runtimeErr(ReasonInvalidParams)
	}
	digest, err := AdmissionFingerprint(target, map[string]any{"input": json.RawMessage(encoded), "intent": intent})
	if err != nil {
		return err
	}
	chainJSON, err := asciiJSONList(chain)
	if err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, `INSERT INTO session_admissions(admission_id,request_id,principal_id,
        target_session_id,lineage_json,payload_json,payload_digest,intent,status,outcome,owner_epoch,generation)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, admissionID, admissionID, principalID, target, chainJSON, encoded, digest, intent,
		status, row["outcome"], epoch, generation); err != nil {
		return err
	}
	if generation != nil {
		_, err = c.ExecContext(ctx, "UPDATE sessions SET runtime_generation=MAX(runtime_generation,?) WHERE id=?", generation, target)
	}
	return err
}

func legacyRows(ctx context.Context, c Conn, query string) ([]map[string]any, error) {
	rows, err := c.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			if _, isBlob := vals[i].([]byte); isBlob {
				return nil, runtimeErr(ReasonInvalidParams)
			}
			row[col] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) ImportLegacySessionAdmissions(ctx context.Context, epoch int64, sourcePath, principalID string, writersDrained bool) (int, error) {
	if !writersDrained {
		return 0, runtimeErr(ReasonInvalidParams)
	}
	if err := checkText(principalID); err != nil {
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
	source, err := s.eng.OpenDB(ctx, resolved, DBOptions{Label: "legacy admissions", ReadOnly: true})
	if err != nil {
		return 0, err
	}
	imported := 0
	err = source.Transact(ctx, false, func(src Conn) error {
		if _, err := src.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
			return err
		}
		rows, err := legacyRows(ctx, src, "SELECT * FROM admissions ORDER BY seq")
		if err != nil {
			return err
		}
		executions := []map[string]any{}
		tables, err := queryStrings(ctx, src, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='executions'")
		if err != nil {
			return err
		}
		if len(tables) > 0 {
			if executions, err = legacyRows(ctx, src, "SELECT * FROM executions ORDER BY root"); err != nil {
				return err
			}
		}
		snapshot, err := canonicalJSON(map[string]any{"rows": rows, "executions": executions})
		if err != nil {
			return runtimeErr(ReasonInvalidParams)
		}
		sum := sha256.Sum256([]byte(snapshot))
		marker, err := canonicalJSON(map[string]any{"source": resolved, "fingerprint": hex.EncodeToString(sum[:]),
			"imported_id_count": len(rows), "principal_id": principalID})
		if err != nil {
			return err
		}
		return s.Write(ctx, WritePatience, func(c Conn) error {
			imported = 0
			if err := checkEpoch(ctx, c, epoch); err != nil {
				return err
			}
			old, found, err := metaValue(ctx, c, legacyImportKey)
			if err != nil {
				return err
			}
			if found {
				if old != marker {
					return runtimeErr(ReasonAdmissionConflict)
				}
				return nil
			}
			for _, execution := range executions {
				generation, ok := execution["generation"].(int64)
				if !ok || generation < 0 {
					return runtimeErr(ReasonInvalidParams)
				}
				root, err := legacyText(execution, "root")
				if err != nil {
					return err
				}
				chain, err := canonicalChainIn(ctx, c, root)
				if err != nil {
					return err
				}
				if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_generation=MAX(runtime_generation,?) WHERE id=?",
					generation, chain[len(chain)-1]); err != nil {
					return err
				}
			}
			for _, row := range rows {
				if err := importLegacyRow(ctx, c, row, epoch, principalID); err != nil {
					return err
				}
			}
			if _, err := c.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?)", legacyImportKey, marker); err != nil {
				return err
			}
			imported = len(rows)
			return nil
		})
	})
	return imported, err
}

const workerColumns = "execution_id, session_id, kind, owner_epoch, generation, status, last_sequence, adoption_digest"

func queryWorker(ctx context.Context, c Conn, executionID string) (*workerRow, error) {
	var w workerRow
	err := c.QueryRowContext(ctx, "SELECT "+workerColumns+" FROM worker_executions WHERE execution_id=?", executionID).
		Scan(&w.ExecutionID, &w.SessionID, &w.Kind, &w.OwnerEpoch, &w.Generation, &w.Status, &w.LastSequence, &w.adoptionDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &w, err
}

func workerAssignment(ctx context.Context, c Conn, executionID, sessionID string, generation int64) (*workerRow, error) {
	row, err := queryWorker(ctx, c, executionID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, runtimeErr(ReasonNotFound)
	}
	if row.SessionID != sessionID {
		return nil, runtimeErr(ReasonPermissionDenied)
	}
	if row.Generation != generation {
		return nil, runtimeErr(ReasonStaleGeneration)
	}
	_, current, err := runtimeCounters(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	if current != generation {
		return nil, runtimeErr(ReasonStaleGeneration)
	}
	return row, nil
}

func linkedWorkerAdmission(ctx context.Context, c Conn, sessionID string, generation, ownerEpoch int64, statuses ...string) (*admissionRow, error) {
	args := append([]any{generation, ownerEpoch}, stringArgs(statuses)...)
	args = append(args, sessionID, sessionID)
	rows, err := queryAdmissions(ctx, c, `generation=? AND owner_epoch=? AND status IN (`+bindMarks(len(statuses))+`)
          AND (target_session_id=? OR EXISTS (
              SELECT 1 FROM json_each(session_admissions.lineage_json) WHERE value=?
          )) ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) > 1 {
		return nil, runtimeErr(ReasonAdmissionConflict)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func anyAdmissionIn(ctx context.Context, c Conn, targets []string, statusClause string) (bool, error) {
	for _, sid := range targets {
		found, err := queryStrings(ctx, c, "SELECT 1 FROM session_admissions WHERE target_session_id=? AND "+statusClause, sid)
		if err != nil {
			return false, err
		}
		if len(found) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) RegisterWorkerExecution(ctx context.Context, epoch int64, executionID, sessionID string, generation int64,
	kind, adoptionSecret string, requireIdle bool) (WorkerExecution, error) {
	if err := checkText(executionID, sessionID); err != nil {
		return WorkerExecution{}, err
	}
	if kind != "cron" && kind != "child" && kind != "compute" && kind != "kanban" {
		return WorkerExecution{}, runtimeErr(ReasonInvalidParams)
	}
	digest, err := secretDigest(adoptionSecret)
	if err != nil {
		return WorkerExecution{}, err
	}
	var out WorkerExecution
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		_, current, err := runtimeCounters(ctx, c, sessionID)
		if err != nil {
			return err
		}
		if current != generation {
			return runtimeErr(ReasonStaleGeneration)
		}
		old, err := queryWorker(ctx, c, executionID)
		if err != nil {
			return err
		}
		if old != nil {
			if old.SessionID != sessionID || old.Generation != generation || old.Kind != kind || old.OwnerEpoch != epoch || old.adoptionDigest != digest {
				return runtimeErr(ReasonAdmissionConflict)
			}
			out = old.WorkerExecution
			return nil
		}
		targets, err := runtimeLineage(ctx, c, sessionID)
		if err != nil {
			return err
		}
		if requireIdle {
			busy, err := anyAdmissionIn(ctx, c, targets, "status IN ('queued','started','unknown')")
			if err != nil {
				return err
			}
			if busy {
				return runtimeErr(ReasonStaleGeneration)
			}
		}
		workers, err := workerStates(ctx, c, sessionID)
		if err != nil {
			return err
		}
		if len(workers) > 0 {
			return runtimeErr(ReasonStaleGeneration)
		}
		unknown, err := anyAdmissionIn(ctx, c, targets, "status='unknown'")
		if err != nil {
			return err
		}
		if unknown {
			return runtimeErr(ReasonUnknownExecution)
		}
		if _, err := c.ExecContext(ctx, `INSERT INTO worker_executions(execution_id,session_id,kind,owner_epoch,generation,status,adoption_digest)
            VALUES(?,?,?,?,?,'registered',?)`, executionID, sessionID, kind, epoch, generation, digest); err != nil {
			return err
		}
		row, err := workerAssignment(ctx, c, executionID, sessionID, generation)
		if err != nil {
			return err
		}
		out = row.WorkerExecution
		return nil
	})
	return out, err
}

func rowsChanged(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) AdoptWorkerExecution(ctx context.Context, epoch int64, executionID, sessionID string, generation int64, adoptionSecret string) (WorkerExecution, error) {
	digest, err := secretDigest(adoptionSecret)
	if err != nil {
		return WorkerExecution{}, err
	}
	var out WorkerExecution
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		row, err := workerAssignment(ctx, c, executionID, sessionID, generation)
		if err != nil {
			return err
		}
		if row.Status == "terminal" {
			return runtimeErr(ReasonStaleGeneration)
		}
		if subtle.ConstantTimeCompare([]byte(row.adoptionDigest), []byte(digest)) != 1 {
			return runtimeErr(ReasonPermissionDenied)
		}
		if strings.HasPrefix(executionID, "admission-worker:") && row.OwnerEpoch != epoch {
			return runtimeErr(ReasonStaleEpoch)
		}
		linked, err := linkedWorkerAdmission(ctx, c, sessionID, generation, row.OwnerEpoch, "started", "unknown")
		if err != nil {
			return err
		}
		if linked != nil {
			n, err := rowsChanged(c.ExecContext(ctx, `UPDATE session_admissions SET owner_epoch=?,status='started'
                WHERE admission_id=? AND generation=? AND owner_epoch=?
                AND status IN ('started','unknown')`, epoch, linked.AdmissionID, generation, row.OwnerEpoch))
			if err != nil {
				return err
			}
			if n != 1 {
				return runtimeErr(ReasonStaleGeneration)
			}
		}
		n, err := rowsChanged(c.ExecContext(ctx, "UPDATE worker_executions SET owner_epoch=?,status='running' WHERE execution_id=?", epoch, executionID))
		if err != nil {
			return err
		}
		if n != 1 {
			return runtimeErr(ReasonStaleGeneration)
		}
		adopted, err := workerAssignment(ctx, c, executionID, sessionID, generation)
		if err != nil {
			return err
		}
		out = adopted.WorkerExecution
		return nil
	})
	return out, err
}

func workerReceiptAssignment(ctx context.Context, c Conn, executionID, sessionID string, generation, sequence int64, digest string) (*workerRow, error) {
	var storedDigest, result string
	err := c.QueryRowContext(ctx, "SELECT payload_digest,result_json FROM worker_receipts WHERE execution_id=? AND sequence=?",
		executionID, sequence).Scan(&storedDigest, &result)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && storedDigest == digest {
		members, _ := objectMembers(json.RawMessage(result))
		assignment, _ := memberValue(members, "worker_assignment")
		fields, ok := objectMembers(assignment)
		parent, _ := memberValue(fields, "parent")
		if ok && len(fields) > 0 && jsonString(parent) == sessionID {
			current, err := queryWorker(ctx, c, executionID)
			if err != nil {
				return nil, err
			}
			if current == nil {
				return nil, fmt.Errorf("worker execution %s vanished", executionID)
			}
			return workerAssignment(ctx, c, executionID, current.SessionID, generation)
		}
	}
	return workerAssignment(ctx, c, executionID, sessionID, generation)
}

func (s *Store) WorkerClaimOrigin(ctx context.Context, executionID, sessionID string) (string, error) {
	origin := sessionID
	err := s.Read(ctx, func(c Conn) error {
		origin = sessionID
		parents, err := queryStrings(ctx, c, "SELECT json_extract(result_json,'$.worker_assignment.parent') FROM worker_receipts WHERE execution_id=? "+
			"AND json_extract(result_json,'$.worker_assignment.parent') IS NOT NULL ORDER BY sequence LIMIT 1", executionID)
		if err == nil && len(parents) > 0 {
			origin = parents[0]
		}
		return err
	})
	return origin, err
}

func workerReceiptDigest(r WorkerReceipt) (string, string, error) {
	encoded, err := canonicalObject(r.Payload)
	if err != nil {
		return "", "", err
	}
	digest, err := AdmissionFingerprint(r.SessionID, map[string]any{"operation": r.Operation, "payload": json.RawMessage(encoded)})
	return encoded, digest, err
}

func (s *Store) WorkerRetryTarget(ctx context.Context, r WorkerReceipt) (string, error) {
	if r.Operation != "compression.publish" {
		return r.SessionID, nil
	}
	_, digest, err := workerReceiptDigest(r)
	if err != nil {
		return "", err
	}
	target := r.SessionID
	err = s.Read(ctx, func(c Conn) error {
		row, err := workerReceiptAssignment(ctx, c, r.ExecutionID, r.SessionID, r.Generation, r.Sequence, digest)
		if err != nil {
			return err
		}
		target = row.SessionID
		return nil
	})
	return target, err
}

func (s *Store) WorkerExecutionRow(ctx context.Context, executionID string) (WorkerExecution, string, bool, error) {
	var out *workerRow
	err := s.Read(ctx, func(c Conn) error {
		var err error
		out, err = queryWorker(ctx, c, executionID)
		return err
	})
	if err != nil || out == nil {
		return WorkerExecution{}, "", false, err
	}
	return out.WorkerExecution, out.adoptionDigest, true, nil
}

func (s *Store) ApplyWorkerReceipt(ctx context.Context, r WorkerReceipt, handlers map[string]WorkerHandler) (json.RawMessage, error) {
	handler, known := handlers[r.Operation]
	if r.Sequence < 1 || !known {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	if handler == nil {
		return nil, &MissingHookError{Name: "worker handler for " + r.Operation}
	}
	encoded, digest, err := workerReceiptDigest(r)
	if err != nil {
		return nil, err
	}
	if len(encoded) > workerPayloadLimit {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	var out json.RawMessage
	err = s.Write(ctx, TranscriptWritePatience, func(c Conn) error {
		out = nil
		if err := checkEpoch(ctx, c, r.Epoch); err != nil {
			return err
		}
		row, err := workerReceiptAssignment(ctx, c, r.ExecutionID, r.SessionID, r.Generation, r.Sequence, digest)
		if err != nil {
			return err
		}
		if row.OwnerEpoch != r.Epoch {
			return runtimeErr(ReasonStaleEpoch)
		}
		var oldDigest, oldResult string
		err = c.QueryRowContext(ctx, "SELECT payload_digest, result_json FROM worker_receipts WHERE execution_id=? AND sequence=?",
			r.ExecutionID, r.Sequence).Scan(&oldDigest, &oldResult)
		if err == nil {
			if oldDigest != digest {
				return runtimeErr(ReasonAdmissionConflict)
			}
			if retiredReceipt(oldResult) {
				return runtimeErr(ReasonStaleGeneration)
			}
			out = json.RawMessage(oldResult)
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if row.Status != "registered" && row.Status != "running" {
			return runtimeErr(ReasonStaleGeneration)
		}
		if r.Sequence != row.LastSequence+1 {
			return runtimeErr(ReasonInvalidParams)
		}
		before, err := totalChanges(ctx, c)
		if err != nil {
			return err
		}
		result, err := handler(ctx, s, c, r.SessionID, json.RawMessage(encoded))
		if err != nil {
			return err
		}
		after, err := totalChanges(ctx, c)
		if err != nil {
			return err
		}
		finish := r.Operation == "execution.finish"
		stored, err := spacedJSON(result)
		if err != nil {
			return runtimeErr(ReasonInvalidParams)
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO worker_receipts(execution_id,sequence,payload_digest,result_json) VALUES(?,?,?,?)",
			r.ExecutionID, r.Sequence, digest, stored); err != nil {
			return err
		}
		status := "running"
		if finish {
			status = "terminal"
		}
		if _, err := c.ExecContext(ctx, "UPDATE worker_executions SET last_sequence=?,status=? WHERE execution_id=?", r.Sequence, status, r.ExecutionID); err != nil {
			return err
		}
		if after != before || finish {
			if _, err := c.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", r.SessionID); err != nil {
				return err
			}
		}
		if finish {
			if err := compactTerminalReceipts(ctx, c, r.ExecutionID); err != nil {
				return err
			}
		}
		out = result
		return nil
	})
	return out, err
}

func (s *Store) MutateWorkerExecution(ctx context.Context, r WorkerReceipt) (json.RawMessage, error) {
	return s.ApplyWorkerReceipt(ctx, r, workerHandlers())
}

func workerHandlers() map[string]WorkerHandler {
	h := map[string]WorkerHandler{
		"session.context":   workerContext,
		"session.prompt":    workerPrompt,
		"session.sidecars":  workerSidecars,
		"session.tools":     workerToolNames,
		"session.tail_role": workerTailRole,
		"session.row_role":  workerRowRole,
		"transcript.append": workerAppend,
		"execution.finish":  workerFinish,
		"usage.main": func(ctx context.Context, s *Store, c Conn, sid string, p json.RawMessage) (json.RawMessage, error) {
			return workerUsage(ctx, c, sid, p, false)
		},
		"usage.auxiliary": func(ctx context.Context, s *Store, c Conn, sid string, p json.RawMessage) (json.RawMessage, error) {
			return workerUsage(ctx, c, sid, p, true)
		},
	}
	for _, op := range []string{"turn.acquire", "turn.renew", "turn.release"} {
		h[op] = func(ctx context.Context, s *Store, c Conn, sid string, p json.RawMessage) (json.RawMessage, error) {
			return workerTurn(ctx, s, c, sid, p, op)
		}
	}
	for name, fn := range workerLifecycleHandlers() {
		h[name] = fn
	}
	for name, fn := range workerCompressionHandlers() {
		h[name] = fn
	}
	return h
}

func payloadKeys(payload json.RawMessage) ([]configMember, map[string]bool, error) {
	members, ok := objectMembers(payload)
	if !ok {
		return nil, nil, runtimeErr(ReasonInvalidParams)
	}
	keys := make(map[string]bool, len(members))
	for _, m := range members {
		keys[m.key] = true
	}
	return members, keys, nil
}

func exactKeys(keys map[string]bool, want ...string) bool {
	if len(keys) != len(want) {
		return false
	}
	for _, k := range want {
		if !keys[k] {
			return false
		}
	}
	return true
}

func keysWithin(keys map[string]bool, allowed ...string) bool {
	for k := range keys {
		if !containsString(allowed, k) {
			return false
		}
	}
	return true
}

func optionalText(members []configMember, key string) (string, bool, error) {
	v, has := memberValue(members, key)
	if !has || jsonIsNull(v) {
		return "", false, nil
	}
	if !jsonIsString(v) {
		return "", false, runtimeErr(ReasonInvalidParams)
	}
	text := jsonString(v)
	return text, true, checkText(text)
}

func workerAppend(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	if !keys["messages"] || !keysWithin(keys, "messages", "turn_lease_holder") {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	raw, _ := memberValue(members, "messages")
	messages, err := decodeWireMessages(ctx, c, sessionID, raw, false)
	if err != nil {
		return nil, err
	}
	holder, _, err := optionalText(members, "turn_lease_holder")
	if err != nil {
		return nil, err
	}
	count, err := s.AppendMessagesIn(ctx, c, sessionID, messages, AppendOptions{TurnLeaseHolder: holder})
	if err != nil {
		return nil, err
	}
	annotations, err := rowAnnotations(messages)
	if err != nil {
		return nil, err
	}
	w := &wireObject{}
	w.set("count", count)
	w.set("annotations", annotations)
	return w.raw()
}

func workerTTL(members []configMember, key string) (float64, error) {
	v, has := memberValue(members, key)
	if !has {
		return 300, nil
	}
	ttl, ok := jsonNumber(v)
	if !ok || ttl < 0.1 || ttl > 3600 {
		return 0, runtimeErr(ReasonInvalidParams)
	}
	return ttl, nil
}

func workerTurn(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage, operation string) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	allowed := exactKeys(keys, "holder", "ttl_seconds")
	if operation == "turn.release" {
		allowed = exactKeys(keys, "holder")
	}
	if !allowed {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	holderRaw, _ := memberValue(members, "holder")
	if !jsonIsString(holderRaw) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	holder := jsonString(holderRaw)
	if err := checkText(holder); err != nil {
		return nil, err
	}
	ttl, err := workerTTL(members, "ttl_seconds")
	if err != nil {
		return nil, err
	}
	key, err := sessionTurnLeaseKeyIn(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	now := nowEpoch()
	switch operation {
	case "turn.acquire":
		acquired, _, err := claimLeaseRow(ctx, c, "session_turn_leases", "conversation_id", key, holder, now, now+ttl,
			func(h string, e float64) bool { return e <= now || s.eng.holderProcessGone(h) })
		if err != nil {
			return nil, err
		}
		return valueResult(acquired)
	case "turn.renew":
		n, err := rowsChanged(c.ExecContext(ctx, "UPDATE session_turn_leases SET expires_at=? WHERE conversation_id=? AND holder=?", now+ttl, key, holder))
		if err != nil {
			return nil, err
		}
		return valueResult(n > 0)
	}
	if _, err := c.ExecContext(ctx, "DELETE FROM session_turn_leases WHERE conversation_id=? AND holder=?", key, holder); err != nil {
		return nil, err
	}
	return valueResult(nil)
}

var modelUsageFields = []string{"model", "billing_provider", "billing_base_url", "billing_mode", "input_tokens", "output_tokens",
	"cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "estimated_cost_usd", "actual_cost_usd", "cost_status",
	"cost_source", "api_call_count"}

var tokenCounterFields = []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "api_call_count"}

func createdSurfaceOf(label string) string {
	switch label {
	case "", "local", "unknown":
		return ""
	case "gui":
		return "desktop"
	}
	return label
}

func workerUsage(ctx context.Context, c Conn, sessionID string, payload json.RawMessage, auxiliary bool) (json.RawMessage, error) {
	members, keys, err := payloadKeys(payload)
	if err != nil {
		return nil, err
	}
	source := ""
	if !auxiliary {
		if v, has := memberValue(members, "source"); has && jsonIsString(v) {
			source = jsonString(v)
		}
		members = dropMember(members, "source")
		delete(keys, "source")
	}
	var allowed []string
	if auxiliary {
		for _, f := range modelUsageFields {
			if f != "billing_mode" && f != "actual_cost_usd" && f != "cost_status" && f != "cost_source" {
				allowed = append(allowed, f)
			}
		}
		allowed = append(allowed, "task")
	} else {
		allowed = append(append(allowed, modelUsageFields...), "pricing_version", "absolute", "task")
	}
	if !keysWithin(keys, allowed...) {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	u := TokenUsage{}
	if auxiliary {
		u.APICallCount = 1
	}
	counters := map[string]*int64{"input_tokens": &u.InputTokens, "output_tokens": &u.OutputTokens, "cache_read_tokens": &u.CacheReadTokens,
		"cache_write_tokens": &u.CacheWriteTokens, "reasoning_tokens": &u.ReasoningTokens, "api_call_count": &u.APICallCount}
	costs := map[string]**float64{"estimated_cost_usd": &u.EstimatedCostUSD, "actual_cost_usd": &u.ActualCostUSD}
	texts := map[string]**string{"model": &u.Model, "billing_provider": &u.BillingProvider, "billing_base_url": &u.BillingBaseURL,
		"billing_mode": &u.BillingMode, "cost_status": &u.CostStatus, "cost_source": &u.CostSource, "pricing_version": &u.PricingVersion}
	for _, m := range members {
		switch {
		case containsString(tokenCounterFields, m.key):
			n, ok := jsonInt(m.value)
			if !ok || n < 0 || n > 1<<53 {
				return nil, runtimeErr(ReasonInvalidParams)
			}
			*counters[m.key] = n
		case costs[m.key] != nil:
			if jsonIsNull(m.value) {
				*costs[m.key] = nil
				continue
			}
			f, ok := jsonNumber(m.value)
			if _, isBool := jsonBool(m.value); !ok || isBool || f < 0 || f > 1e12 {
				return nil, runtimeErr(ReasonInvalidParams)
			}
			*costs[m.key] = &f
		case m.key == "absolute":
			b, ok := jsonBool(m.value)
			if !ok {
				return nil, runtimeErr(ReasonInvalidParams)
			}
			u.Absolute = b
		default:
			if jsonIsNull(m.value) {
				continue
			}
			if !jsonIsString(m.value) || len([]rune(jsonString(m.value))) > 4096 {
				return nil, runtimeErr(ReasonInvalidParams)
			}
			text := jsonString(m.value)
			if m.key == "task" {
				u.Task = text
			} else {
				*texts[m.key] = &text
			}
		}
	}
	if auxiliary {
		if u.Task == "" {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		if err := recordModelUsageIn(ctx, c, sessionID, u); err != nil {
			return nil, err
		}
		return valueResult(nil)
	}
	if err := updateTokenCountsIn(ctx, c, sessionID, u); err != nil {
		return nil, err
	}
	if surfaced := createdSurfaceOf(source); surfaced != "" {
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET created_source=? WHERE id=? AND "+unsetCreatedSQL("created_source"), surfaced, sessionID); err != nil {
			return nil, err
		}
		if err := stampCompressionAncestors(ctx, c, sessionID); err != nil {
			return nil, err
		}
	}
	return valueResult(nil)
}

func workerFinish(ctx context.Context, s *Store, c Conn, sessionID string, payload json.RawMessage) (json.RawMessage, error) {
	members, ok := objectMembers(payload)
	if !ok || len(members) > 0 {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	w := &wireObject{}
	w.set("status", "terminal")
	return w.raw()
}
