package state

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// Separator between a parent execution id and the random suffix of a delegated child
const ChildExecutionSeparator = "/child:"

// Receipt text a terminal execution's read copies shrink to
const retiredReceiptJSON = `{"retired": true}`

// Every session id worker exclusion covers for a conversation, its logical owner, physical targets and compression lineage
func runtimeLineage(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	targets := map[string]bool{sessionID: true}
	physical, err := localPhysicalTarget(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	targets[physical] = true
	owner, found, err := chatIDOf(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	if found && owner != "" {
		receipt, err := storedLocalReceipt(ctx, c, owner)
		if err != nil {
			return nil, err
		}
		if receipt != nil {
			lineage, ok := receiptLineage(receipt, owner)
			if !ok {
				return nil, runtimeErr(ReasonStorageUnavailable)
			}
			if containsString(lineage, sessionID) {
				targets[owner] = true
				ownerTarget, err := localPhysicalTarget(ctx, c, owner)
				if err != nil {
					return nil, err
				}
				targets[ownerTarget] = true
			}
		}
	}
	frontier := make([]string, 0, len(targets))
	for id := range targets {
		frontier = append(frontier, id)
	}
	for range chainCap {
		found, err := expandCompressionLineageIDs(ctx, c, frontier)
		if err != nil {
			return nil, err
		}
		frontier = frontier[:0]
		for _, id := range found {
			if !targets[id] {
				targets[id] = true
				frontier = append(frontier, id)
			}
		}
		if len(frontier) == 0 {
			out := make([]string, 0, len(targets))
			for id := range targets {
				out = append(out, id)
			}
			sort.Strings(out)
			return out, nil
		}
	}
	return nil, runtimeErr(ReasonStorageUnavailable)
}

// chat_id of a session row, false when the row is missing
func chatIDOf(ctx context.Context, c Conn, sessionID string) (string, bool, error) {
	values, err := queryStrings(ctx, c, "SELECT chat_id FROM sessions WHERE id=?", sessionID)
	if err != nil || len(values) == 0 {
		return "", false, err
	}
	return values[0], true, nil
}

// Live worker statuses anywhere in the conversation's lineage
func workerStates(ctx context.Context, c Conn, sessionID string) ([]string, error) {
	lineage, err := runtimeLineage(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(lineage)
	if err != nil {
		return nil, err
	}
	return queryStrings(ctx, c, "SELECT DISTINCT status FROM worker_executions WHERE session_id IN (SELECT value FROM json_each(?)) "+
		"AND status IN ('registered','running','unknown')", string(encoded))
}

// Revokes unlinked uncertain compute on an explicit reset, admission linked work keeping its own discard receipt
func discardOrphanWorkersOnReset(ctx context.Context, c Conn, sessionIDs []string) error {
	for _, sid := range sessionIDs {
		orphans, err := queryStrings(ctx, c, "SELECT execution_id FROM worker_executions WHERE session_id=? AND status='unknown' "+
			"AND execution_id NOT LIKE 'admission-worker:%'", sid)
		if err != nil {
			return err
		}
		for _, executionID := range orphans {
			if _, err := c.ExecContext(ctx, "UPDATE worker_executions SET status='terminal' WHERE execution_id=?", executionID); err != nil {
				return err
			}
			if err := compactTerminalReceipts(ctx, c, executionID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Primary key bounds of every descendant id of an execution
func descendantRange(executionID string) (string, string) {
	return executionID + ChildExecutionSeparator, executionID + strings.TrimSuffix(ChildExecutionSeparator, ":") + ";"
}

// Retires every live descendant of an execution with it
func retireChildExecutions(ctx context.Context, c Conn, executionID string) error {
	low, high := descendantRange(executionID)
	children, err := queryStrings(ctx, c, "SELECT execution_id FROM worker_executions "+
		"WHERE execution_id>=? AND execution_id<? AND status!='terminal'", low, high)
	if err != nil {
		return err
	}
	return retireChildren(ctx, c, children)
}

// Retires the children of an earlier owner epoch at restart, a child never being adoptable
func retireStaleChildren(ctx context.Context, c Conn, epoch int64) error {
	children, err := queryStrings(ctx, c, "SELECT execution_id FROM worker_executions WHERE kind='child' "+
		"AND status!='terminal' AND owner_epoch!=?", epoch)
	if err != nil {
		return err
	}
	return retireChildren(ctx, c, children)
}

// Makes each child terminal, shrinks its read receipts and ends its open session row
func retireChildren(ctx context.Context, c Conn, children []string) error {
	for _, child := range children {
		if _, err := c.ExecContext(ctx, "UPDATE worker_executions SET status='terminal' WHERE execution_id=?", child); err != nil {
			return err
		}
		if err := compactReceipts(ctx, c, child); err != nil {
			return err
		}
		if err := endChildRow(ctx, c, child); err != nil {
			return err
		}
	}
	return nil
}

// Ends the open session row of a child execution as an agent close
func endChildRow(ctx context.Context, c Conn, executionID string) error {
	_, err := c.ExecContext(ctx, "UPDATE sessions SET ended_at=?,end_reason='agent_close' WHERE ended_at IS NULL "+
		"AND id=(SELECT session_id FROM worker_executions WHERE execution_id=?)", nowEpoch(), executionID)
	return err
}

// Shrinks the read receipts of an execution going terminal and retires its delegated children
func compactTerminalReceipts(ctx context.Context, c Conn, executionID string) error {
	if err := retireChildExecutions(ctx, c, executionID); err != nil {
		return err
	}
	if err := compactReceipts(ctx, c, executionID); err != nil {
		return err
	}
	if strings.Contains(executionID, ChildExecutionSeparator) {
		return endChildRow(ctx, c, executionID)
	}
	return nil
}

// Replaces every transcript or session carrying receipt of an execution with the retired marker
func compactReceipts(ctx context.Context, c Conn, executionID string) error {
	_, err := c.ExecContext(ctx, "UPDATE worker_receipts SET result_json=? WHERE execution_id=? AND json_valid(result_json) "+
		"AND (json_type(result_json, '$.messages') IS NOT NULL "+
		"OR json_type(result_json, '$.session') IS NOT NULL)", retiredReceiptJSON, executionID)
	return err
}

// Reports a stored receipt result that is the retired marker
func retiredReceipt(resultJSON string) bool {
	members, ok := objectMembers(json.RawMessage(resultJSON))
	if !ok || len(members) != 1 {
		return false
	}
	v, has := memberValue(members, "retired")
	return has && strings.TrimSpace(string(v)) == "true"
}
