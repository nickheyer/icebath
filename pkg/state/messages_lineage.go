package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Whether a session is a branch, delegate or tool child of its parent, reset forks too on request
func isExplicitForkChildRow(session *pyjson.Object, includeReset bool) bool {
	if pyEquals(rowValue(session, "source"), "tool") {
		return true
	}
	raw := rowValue(session, "model_config")
	if text, ok := raw.(string); ok {
		parsed, err := pyjson.Loads(text)
		if err != nil {
			return false
		}
		raw = parsed
	}
	cfg, ok := raw.(*pyjson.Object)
	if !ok {
		return false
	}
	keys := []string{"_branched_from", "_delegate_from"}
	if includeReset {
		keys = append(keys, "_reset_from")
	}
	markers := make([]any, len(keys))
	for i, k := range keys {
		markers[i], _ = cfg.Get(k)
	}
	parentID := rowValue(session, "parent_session_id")
	if pyTruthy(parentID) {
		for _, m := range markers {
			if pyValueEqual(m, parentID) {
				return true
			}
		}
		return false
	}
	for _, m := range markers {
		if m != nil {
			return true
		}
	}
	return false
}

// Read only fork classification of a stored session, a missing row is no fork
func (d *SessionDB) IsExplicitForkChild(ctx context.Context, sessionID string) (bool, error) {
	session, err := d.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		return false, err
	}
	return isExplicitForkChildRow(session, false), nil
}

// Verified compression lineage root to tip that a display resume materializes, or the session alone
func (d *SessionDB) resumeLineageIDs(ctx context.Context, sessionID string) ([]string, error) {
	if sessionID == "" {
		return []string{sessionID}, nil
	}
	session, err := d.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session == nil || isExplicitForkChildRow(session, false) {
		return []string{sessionID}, nil
	}
	chain := []string{sessionID}
	current := session
	seen := map[string]bool{sessionID: true}
	for len(chain) < 100 {
		child, err := d.isCompressionChildRow(ctx, current)
		if err != nil {
			return nil, err
		}
		if !child {
			break
		}
		parentID, _ := rowValue(current, "parent_session_id").(string)
		parent, err := d.GetSession(ctx, parentID)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			break
		}
		id := pyStr(rowValue(parent, "id"))
		if seen[id] {
			break
		}
		seen[id] = true
		chain = append(chain, id)
		current = parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// Whether the session or any segment of its resume lineage belongs to the Kanban dispatcher
func (d *SessionDB) IsKanbanOwnedSession(ctx context.Context, sessionID string) (bool, error) {
	lineage, err := d.resumeLineageIDs(ctx, sessionID)
	if err != nil || len(lineage) == 0 {
		return false, err
	}
	args := make([]any, len(lineage))
	for i, id := range lineage {
		args[i] = id
	}
	rows, err := d.readAll(ctx, "SELECT 1 FROM sessions WHERE id IN ("+placeholders(len(lineage))+") "+
		"AND (source = 'kanban' OR created_source = 'kanban') LIMIT 1", args...)
	return len(rows) > 0, err
}
