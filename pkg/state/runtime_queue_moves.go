package state

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// State meta key prefixes of the queued replacement bindings and the mutation exact retry receipts
const (
	ReplacementPrefix      = "gateway.queued_replacement.v1."
	mutationReceiptPrefix  = "gateway.mutation.v1."
	cancelQueuedAdmissionQ = "UPDATE session_admissions SET status='terminal',outcome='cancelled' WHERE admission_id=?"
)

// Durable key binding a replacement identity to the admission it retired
func ReplacementBinding(principalID, sessionID, requestID string) (string, error) {
	digest, err := AdmissionFingerprint(sessionID, map[string]any{"principal": principalID, "request": requestID})
	if err != nil {
		return "", err
	}
	return ReplacementPrefix + digest, nil
}

// Cancels the still queued replaced row and binds it to its successor inside the successor admission write
func RetireReplacedInput(ctx context.Context, conn SQLiteConn, admissionID, binding string) error {
	row, err := runtimeAdmission(ctx, conn, admissionID)
	if err != nil {
		return err
	}
	if !pyjson.Equal(rowValue(row, "status"), "queued") {
		return runtimeRefusal("admission_started")
	}
	if _, err := conn.ExecContext(ctx, cancelQueuedAdmissionQ, admissionID); err != nil {
		return err
	}
	encoded, err := pyDumps(admissionID, pyJSONDefault)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "INSERT OR REPLACE INTO state_meta(key,value) VALUES(?,?)", binding, encoded)
	return err
}

// Refuses an exact replacement replay unless its binding names the admission it retired
func CheckReplacedInput(ctx context.Context, conn SQLiteConn, admissionID, binding string) error {
	saved, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", binding)
	if err != nil {
		return err
	}
	if saved == nil {
		return runtimeRefusal("admission_conflict")
	}
	text, _ := saved["value"].(string)
	value, err := pyjson.Loads(text)
	if err != nil {
		return pyValueError(err.Error())
	}
	if !pyjson.Equal(value, admissionID) {
		return runtimeRefusal("admission_conflict")
	}
	return nil
}

// Admissions still queued behind no unknown head, in FIFO order
func queuedAdmissionsBehindKnownHead(ctx context.Context, conn SQLiteConn, ownerSessionID, query string, args ...any) ([]*pyjson.Object, error) {
	unknown, err := connRow(ctx, conn, "SELECT 1 FROM session_admissions WHERE target_session_id=? AND status='unknown'", ownerSessionID)
	if err != nil || unknown != nil {
		return nil, err
	}
	return queryObjects(ctx, conn, query, args...)
}

// Moves the old owner's queued prefix admissions onto the new session keeping ids and seq, returning them in FIFO order
func (d *SessionDB) RehomeQueuedInputs(ctx context.Context, epoch int64, oldSessionID, newSessionID, principalPrefix string) ([]string, error) {
	for _, v := range []string{oldSessionID, newSessionID, principalPrefix} {
		if err := runtimeText(v); err != nil {
			return nil, err
		}
	}
	moved := []string{}
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		moved = []string{}
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		if _, err := runtimeSession(ctx, conn, newSessionID); err != nil {
			return err
		}
		rows, err := queuedAdmissionsBehindKnownHead(ctx, conn, oldSessionID,
			"SELECT * FROM session_admissions WHERE target_session_id=? AND status='queued' "+
				"AND substr(principal_id,1,?)=? ORDER BY seq", oldSessionID, len([]rune(principalPrefix)), principalPrefix)
		if err != nil {
			return err
		}
		lineage, err := pyDumps([]any{newSessionID}, pyJSONDefault)
		if err != nil {
			return err
		}
		for _, row := range rows {
			text, _ := rowValue(row, "payload_json").(string)
			input, err := pyjson.Loads(text)
			if err != nil {
				return pyValueError(err.Error())
			}
			digest, err := AdmissionFingerprint(newSessionID, map[string]any{"input": input, "intent": rowValue(row, "intent")})
			if err != nil {
				return err
			}
			admissionID := rowValue(row, "admission_id")
			if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET target_session_id=?,lineage_json=?,payload_digest=? "+
				"WHERE admission_id=? AND status='queued'", newSessionID, lineage, digest, admissionID); err != nil {
				return err
			}
			moved = append(moved, fmt.Sprint(admissionID))
		}
		if len(rows) > 0 {
			for _, sid := range []string{oldSessionID, newSessionID} {
				if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", sid); err != nil {
					return err
				}
			}
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return moved, nil
}

// Points a local owner's queued automation rows naming a pre reset transcript at the new target, returning them in FIFO order
func (d *SessionDB) RetargetLocalAutomation(ctx context.Context, epoch int64, sessionID, principalID string, oldTargets []string, newTarget string) ([]string, error) {
	for _, v := range append([]string{sessionID, principalID, newTarget}, oldTargets...) {
		if err := runtimeText(v); err != nil {
			return nil, err
		}
	}
	old := make(map[string]bool, len(oldTargets))
	for _, t := range oldTargets {
		old[t] = true
	}
	moved := []string{}
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		moved = []string{}
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		if _, err := runtimeSession(ctx, conn, newTarget); err != nil {
			return err
		}
		rows, err := queuedAdmissionsBehindKnownHead(ctx, conn, sessionID,
			"SELECT * FROM session_admissions WHERE target_session_id=? AND status='queued' "+
				"AND principal_id=? ORDER BY seq", sessionID, principalID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			text, _ := rowValue(row, "payload_json").(string)
			decoded, err := pyjson.Loads(text)
			if err != nil {
				return pyValueError(err.Error())
			}
			payload, _ := decoded.(*pyjson.Object)
			var descriptor *pyjson.Object
			if payload != nil {
				raw, _ := payload.Get("local_automation_v1")
				descriptor, _ = raw.(*pyjson.Object)
			}
			if descriptor == nil {
				continue
			}
			target, _ := descriptor.Get("target")
			switch target.(type) {
			case []any:
				return &pyConvError{"TypeError", "unhashable type: 'list'"}
			case *pyjson.Object:
				return &pyConvError{"TypeError", "unhashable type: 'dict'"}
			}
			if name, ok := target.(string); !ok || !old[name] {
				continue
			}
			descriptor.Set("target", newTarget)
			digest, err := AdmissionFingerprint(sessionID, map[string]any{"input": payload, "intent": rowValue(row, "intent")})
			if err != nil {
				return err
			}
			encoded, err := runtimeJSON(payload)
			if err != nil {
				return err
			}
			admissionID := rowValue(row, "admission_id")
			if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET payload_json=?,payload_digest=? WHERE admission_id=? AND status='queued'",
				encoded, digest, admissionID); err != nil {
				return err
			}
			moved = append(moved, fmt.Sprint(admissionID))
		}
		if len(moved) > 0 {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", sessionID); err != nil {
				return err
			}
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return moved, nil
}

// State meta key and digest of a mutation request's durable exact retry receipt
func MutationReceiptKey(principalID, sessionID, requestID, operation string, payload any, expectedRevision int64, expectedGeneration *int64) (string, string, error) {
	keyDigest, err := AdmissionFingerprint(sessionID, map[string]any{"principal": principalID, "request": requestID})
	if err != nil {
		return "", "", err
	}
	fingerprint := map[string]any{"operation": operation, "payload": payload, "expected_revision": expectedRevision}
	if expectedGeneration != nil {
		fingerprint["expected_generation"] = *expectedGeneration
	}
	digest, err := AdmissionFingerprint(sessionID, fingerprint)
	if err != nil {
		return "", "", err
	}
	return mutationReceiptPrefix + keyDigest, digest, nil
}

// Stored result of an exact mutation retry, nil when unseen, refusing a different request under the key
func ReplayMutationReceipt(ctx context.Context, conn SQLiteConn, key, digest string) (any, error) {
	old, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", key)
	if err != nil || old == nil {
		return nil, err
	}
	text, _ := old["value"].(string)
	receipt, err := pyjson.Loads(text)
	if err != nil {
		return nil, pyValueError(err.Error())
	}
	stored, err := receiptItem(receipt, "digest")
	if err != nil {
		return nil, err
	}
	if !pyjson.Equal(stored, digest) {
		return nil, runtimeRefusal("admission_conflict")
	}
	return receiptItem(receipt, "result")
}
