package state

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Workspace grouping key of a session row, git repo root else cwd, the branch never fragmenting history
func WorkspaceKey(row any) (string, bool) {
	for _, key := range []string{"git_repo_root", "cwd"} {
		if v, ok := mappingGet(row, key); ok && pyTruthy(v) {
			if s := pystr.Strip(pyStr(v)); s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// Mirrors the SessionEntry expiry_finalized flag onto the row so it survives a lost sessions.json
func (d *SessionDB) SetExpiryFinalized(ctx context.Context, sessionID string, finalized bool) error {
	if sessionID == "" {
		return nil
	}
	flag := 0
	if finalized {
		flag = 1
	}
	return d.writeSQL(ctx, "UPDATE sessions SET expiry_finalized = ? WHERE id = ?", []any{flag, sessionID}, false, nil)
}

// Durably marks a reset boundary on a live row or a recoverably ended one, false when nothing was written
func (d *SessionDB) PromoteToSessionReset(ctx context.Context, sessionID, reason string) bool {
	if sessionID == "" {
		return false
	}
	at := now()
	var changed int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		changed, err = d.endAndBump(ctx, conn, "UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND (ended_at IS NULL "+
			"OR end_reason IN ("+recoverableEndReasonsSQL+"))", []any{at, reason, sessionID}, sessionID, reason)
		return err
	}, nil)
	return err == nil && changed > 0
}

// Replaces model_config and, when given, the model after draining queued token deltas
func (d *SessionDB) UpdateSessionMeta(ctx context.Context, sessionID, modelConfigJSON string, model *string) error {
	d.FlushTokenCounts(5.0)
	var modelArg any
	if model != nil {
		modelArg = *model
	}
	return d.writeSQL(ctx, "UPDATE sessions SET model_config = ?, model = COALESCE(?, model) WHERE id = ?",
		[]any{modelConfigJSON, modelArg, sessionID}, false, nil)
}

// Browser or API client runtime lock fields, nil strings meaning unset
type RuntimeLock struct {
	Model, Provider, RouteSource *string
	ModelOptions                 any
	Confirmed                    bool
}

// Persists a Browser or API client runtime lock into model_config, lineage markers surviving
func (d *SessionDB) UpdateSessionRuntimeLock(ctx context.Context, sessionID string, lock RuntimeLock) error {
	text := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	options := lock.ModelOptions
	if !pyTruthy(options) {
		options = pyjson.NewObject()
	}
	patch := pyjson.ObjectOf("browser_model_lock", pyjson.ObjectOf(
		"provider", text(lock.Provider), "model", text(lock.Model), "model_options", options,
		"route_source", text(lock.RouteSource), "confirmed", lock.Confirmed, "updated_at", now()))
	var modelArg any
	if lock.Model != nil {
		modelArg = *lock.Model
	}
	return d.writeModelConfigPatch(ctx, sessionID, patch, `UPDATE sessions SET
                   model_config = ?,
                   model = COALESCE(?, model)
                   WHERE id = ?`, func(merged any) []any { return []any{merged, modelArg, sessionID} })
}

// Picker columns of one session without the prompt join or the token flush, nil when absent
func (d *SessionDB) GetSessionListRow(ctx context.Context, sessionID string) (*pyjson.Object, error) {
	return d.readOne(ctx, "SELECT id, source, title, started_at, ended_at, message_count, model, hidden, archived "+
		"FROM sessions WHERE id = ?", sessionID)
}

// Un-archives a session ended by a recoverable accident, true only when the lineage was un-archived
func (d *SessionDB) UnarchiveRecoverableSession(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	row, err := d.GetSession(ctx, sessionID)
	if err != nil || row == nil || !pyTruthy(rowValue(row, "archived")) {
		return false, nil
	}
	tip := row
	if tipID, err := d.GetCompressionTip(ctx, sessionID); err == nil {
		if tipID == "" {
			tipID = sessionID
		}
		if tipID != sessionID {
			if other, err := d.GetSession(ctx, tipID); err == nil && other != nil {
				tip = other
			}
		}
	}
	reason, _ := rowValue(tip, "end_reason").(string)
	if !slices.Contains(recoverableEndReasons, reason) {
		return false, nil
	}
	ok, err := d.SetSessionArchived(ctx, sessionID, false, true)
	if err != nil || !ok {
		return false, err
	}
	if err := d.writeSQL(ctx, "UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ?", []any{rowValue(tip, "id")}, false, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Export row count of a segment, every row counted, refusing above the bound, zero disabling the guard
func (d *SessionDB) AssertExportSafe(ctx context.Context, sessionID string, maxMessages *int) (int, error) {
	limit := ResolvedMaxExportMessages(ctx)
	if maxMessages != nil {
		limit = *maxMessages
	}
	if limit < 0 {
		return 0, pyValueError("max_messages must be non-negative")
	}
	if limit == 0 {
		return 0, nil
	}
	row, err := d.readOne(ctx, "SELECT COUNT(*) FROM (SELECT 1 FROM messages WHERE session_id = ? LIMIT ?)", sessionID, limit+1)
	if err != nil {
		return 0, err
	}
	count := 0
	if row != nil && len(row.Keys) > 0 {
		n, _ := exactInt(row.Vals[row.Keys[0]])
		count = int(n)
	}
	if count > limit {
		return 0, &SessionExportTooLargeError{SessionID: sessionID, MessageCount: count, Limit: limit}
	}
	return count, nil
}

// AssertExportSafe over every id with the bound resolved once, zero disabling without any query
func (d *SessionDB) AssertExportsSafe(ctx context.Context, sessionIDs []string, maxMessages *int) error {
	limit := ResolvedMaxExportMessages(ctx)
	if maxMessages != nil {
		limit = *maxMessages
	}
	if limit == 0 {
		return nil
	}
	for _, sessionID := range sessionIDs {
		if _, err := d.AssertExportSafe(ctx, sessionID, &limit); err != nil {
			return err
		}
	}
	return nil
}
