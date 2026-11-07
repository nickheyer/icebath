package state

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Separator between a delegated child's execution id and its parent's, so descendants share one prefix
const ChildSeparator = "/child:"

const retiredReceiptJSON = `{"retired": true}`

// Logical owner, physical transcript and every compression relative of a session, refused past the chain cap
func runtimeLineage(ctx context.Context, conn SQLiteConn, sessionID string) (map[string]bool, error) {
	physical, err := localPhysicalTarget(ctx, conn, sessionID)
	if err != nil {
		return nil, err
	}
	targets := map[string]bool{sessionID: true, physical: true}
	row, err := connRow(ctx, conn, "SELECT chat_id FROM sessions WHERE id=?", sessionID)
	if err != nil {
		return nil, err
	}
	if chatID, _ := row["chat_id"].(string); row != nil && chatID != "" {
		saved, err := connRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+chatID)
		if err != nil {
			return nil, err
		}
		if saved != nil {
			owned, err := receiptLineageHolds(saved["value"], chatID, sessionID)
			if err != nil {
				return nil, err
			}
			if owned {
				ownerTarget, err := localPhysicalTarget(ctx, conn, chatID)
				if err != nil {
					return nil, err
				}
				targets[chatID], targets[ownerTarget] = true, true
			}
		}
	}
	frontier := make([]string, 0, len(targets))
	for sid := range targets {
		frontier = append(frontier, sid)
	}
	for range chainCap {
		found, err := expandCompressionLineageIDs(ctx, conn, frontier)
		if err != nil {
			return nil, err
		}
		frontier = frontier[:0]
		for _, sid := range found {
			if !targets[sid] {
				frontier = append(frontier, sid)
			}
		}
		if len(frontier) == 0 {
			return targets, nil
		}
		for _, sid := range frontier {
			targets[sid] = true
		}
	}
	return nil, runtimeRefusal("storage_unavailable")
}

// True when the stored receipt lineage, defaulting to the owner alone, lists the session
func receiptLineageHolds(raw any, owner, sessionID string) (bool, error) {
	text, _ := raw.(string)
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return false, pyValueError(err.Error())
	}
	receipt, ok := decoded.(*pyjson.Object)
	if !ok {
		return false, &stateTypedError{"AttributeError", "receipt has no attribute get"}
	}
	lineage, present := receipt.Get("lineage")
	if !present {
		return owner == sessionID, nil
	}
	items, ok := lineage.([]any)
	if !ok {
		if s, isStr := lineage.(string); isStr {
			return strings.Contains(s, sessionID), nil
		}
		if keyed, isObj := lineage.(*pyjson.Object); isObj {
			_, held := keyed.Get(sessionID)
			return held, nil
		}
		return false, &stateTypedError{"TypeError", "argument of type is not iterable"}
	}
	for _, item := range items {
		if pyEquals(item, sessionID) {
			return true, nil
		}
	}
	return false, nil
}

// Live worker statuses across the session's whole runtime lineage in one indexed probe
func workerStates(ctx context.Context, conn SQLiteConn, sessionID string) (map[string]bool, error) {
	lineage, err := runtimeLineage(ctx, conn, sessionID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(lineage))
	for sid := range lineage {
		ids = append(ids, sid)
	}
	slices.Sort(ids)
	encoded, err := pyjson.Dumps(stringsToAny(ids))
	if err != nil {
		return nil, err
	}
	statuses, err := queryStrings(ctx, conn, "SELECT DISTINCT status FROM worker_executions WHERE session_id IN (SELECT value FROM json_each(?)) "+
		"AND status IN ('registered','running','unknown')", encoded)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, s := range statuses {
		out[s] = true
	}
	return out, nil
}

// Reset revokes unlinked unknown compute on each session and compacts its receipts
func DiscardOrphanWorkersOnReset(ctx context.Context, conn SQLiteConn, sessionIDs []string) error {
	for _, sid := range sessionIDs {
		orphans, err := queryStrings(ctx, conn, "SELECT execution_id FROM worker_executions WHERE session_id=? AND status='unknown' "+
			"AND execution_id NOT LIKE 'admission-worker:%'", sid)
		if err != nil {
			return err
		}
		for _, executionID := range orphans {
			if _, err := conn.ExecContext(ctx, "UPDATE worker_executions SET status='terminal' WHERE execution_id=?", executionID); err != nil {
				return err
			}
			if err := compactTerminalReceipts(ctx, conn, executionID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Retires every live descendant of an execution, shrinking receipts and ending its session row
func retireChildExecutions(ctx context.Context, conn SQLiteConn, executionID string) error {
	low, high := DescendantRange(executionID)
	rows, err := queryStrings(ctx, conn, "SELECT execution_id FROM worker_executions "+
		"WHERE execution_id>=? AND execution_id<? AND status!='terminal'", low, high)
	if err != nil {
		return err
	}
	return retireChildren(ctx, conn, rows)
}

// Primary key bounds holding every descendant id of the execution
func DescendantRange(executionID string) (string, string) {
	return executionID + ChildSeparator, executionID + ChildSeparator[:len(ChildSeparator)-1] + ";"
}

// Owner restart retires every live child execution another epoch started
func retireStaleChildren(ctx context.Context, conn SQLiteConn, epoch int64) error {
	rows, err := queryStrings(ctx, conn, "SELECT execution_id FROM worker_executions WHERE kind='child' "+
		"AND status!='terminal' AND owner_epoch!=?", epoch)
	if err != nil {
		return err
	}
	return retireChildren(ctx, conn, rows)
}

// Marks each child terminal, compacts its receipts and ends its open session row
func retireChildren(ctx context.Context, conn SQLiteConn, children []string) error {
	for _, child := range children {
		if _, err := conn.ExecContext(ctx, "UPDATE worker_executions SET status='terminal' WHERE execution_id=?", child); err != nil {
			return err
		}
		if err := compactReceipts(ctx, conn, child); err != nil {
			return err
		}
		if err := endChildRow(ctx, conn, child); err != nil {
			return err
		}
	}
	return nil
}

// Ends the still open session row a child execution ran in
func endChildRow(ctx context.Context, conn SQLiteConn, executionID string) error {
	_, err := conn.ExecContext(ctx, "UPDATE sessions SET ended_at=?,end_reason='agent_close' WHERE ended_at IS NULL "+
		"AND id=(SELECT session_id FROM worker_executions WHERE execution_id=?)", now(), executionID)
	return err
}

// Retires an execution's children and shrinks its history bearing read receipts to the retired marker
func compactTerminalReceipts(ctx context.Context, conn SQLiteConn, executionID string) error {
	if err := retireChildExecutions(ctx, conn, executionID); err != nil {
		return err
	}
	if err := compactReceipts(ctx, conn, executionID); err != nil {
		return err
	}
	if strings.Contains(executionID, ChildSeparator) {
		return endChildRow(ctx, conn, executionID)
	}
	return nil
}

// Replaces transcript or session bearing receipt results with the retired marker
func compactReceipts(ctx context.Context, conn SQLiteConn, executionID string) error {
	_, err := conn.ExecContext(ctx, "UPDATE worker_receipts SET result_json=? WHERE execution_id=? AND json_valid(result_json) "+
		"AND (json_type(result_json, '$.messages') IS NOT NULL "+
		"OR json_type(result_json, '$.session') IS NOT NULL)", retiredReceiptJSON, executionID)
	return err
}

// True when a stored receipt result is exactly the retired marker
func RetiredReceipt(resultJSON string) (bool, error) {
	decoded, err := pyjson.Loads(resultJSON)
	if err != nil {
		return false, pyValueError(err.Error())
	}
	obj, ok := decoded.(*pyjson.Object)
	return ok && obj.Len() == 1 && pyjson.Equal(obj, pyjson.ObjectOf("retired", true)), nil
}
