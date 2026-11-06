package state

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const archiveActiveSQL = "UPDATE messages SET active = 0, compacted = 1 WHERE session_id = ? AND active = 1"

// Keyword arguments of an in place compaction, nil standing for an omitted Python argument
type ArchiveCompactOptions struct {
	ModelConfigPatch *pyjson.Object
	Watermark        *int64
	LockHolder       *string
	TailCount        int
	CarriedMessages  []map[string]any
	CoveredIDs       []int64
	UnresolvedHeld   []map[string]any
	HeldForCAS       []map[string]any
}

// Ids and tool call count of the concurrent tail rows the query selects
func tailRowsAfterWatermark(ctx context.Context, conn SQLiteConn, query string, args ...any) ([]int64, int, error) {
	rows, err := connRows(ctx, conn, query, args...)
	if err != nil {
		return nil, 0, err
	}
	toolCalls := 0
	for _, row := range rows {
		toolCalls += toolCallsLen(row["tool_calls"], 0)
	}
	return rowIDList(rows), toolCalls, nil
}

// Pure SQL clone of the rows as fresh live rows, payload columns byte exact, into sessionID when given
func (d *SessionDB) cloneMessageRows(ctx context.Context, conn SQLiteConn, tailIDs []int64, sessionID *string) error {
	skip := map[string]bool{"id": true, "active": true, "compacted": true, "display_order": true}
	if sessionID != nil {
		skip["session_id"] = true
	}
	columns, err := d.messageColumnNames(ctx, conn)
	if err != nil {
		return err
	}
	var kept []string
	for _, column := range columns {
		if !skip[column] {
			kept = append(kept, column)
		}
	}
	colList := strings.Join(kept, ", ")
	args := int64sToAny(tailIDs)
	target, value := "", ""
	if sessionID != nil {
		target, value = "session_id, ", "?, "
		args = append([]any{*sessionID}, args...)
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO messages ("+colList+", "+target+"active, compacted) SELECT "+colList+", "+value+"1, 0 FROM messages WHERE id IN ("+placeholders(len(tailIDs))+") ORDER BY id", args...)
	return err
}

func (d *SessionDB) setCompactedCounters(ctx context.Context, conn SQLiteConn, sessionID string, inserted, toolCalls int, patched any, patch bool) error {
	if patch {
		_, err := conn.ExecContext(ctx, setCountersSQL+", model_config = ? WHERE id = ?", inserted, toolCalls, patched, sessionID)
		return err
	}
	_, err := conn.ExecContext(ctx, setCountersSQL+" WHERE id = ?", inserted, toolCalls, sessionID)
	return err
}

func rewindRows(ctx context.Context, conn SQLiteConn, sessionID string, rewindIDs []int64) error {
	_, err := conn.ExecContext(ctx, "UPDATE messages SET active = 0, compacted = 0 WHERE session_id = ? AND id IN ("+placeholders(len(rewindIDs))+")", append([]any{sessionID}, int64sToAny(rewindIDs)...)...)
	return err
}

// Archives covered rows as summarized and clones every other active row after the new compacted set
func (d *SessionDB) archiveNamedRows(ctx context.Context, conn SQLiteConn, sessionID string, compacted []map[string]any, covered []int64, o ArchiveCompactOptions, patched any, patch bool, mergedAway map[int64]bool) (int, error) {
	activeRows, err := connRows(ctx, conn, activeIDsSQL, sessionID)
	if err != nil {
		return 0, err
	}
	activeIDs := rowIDList(activeRows)
	coveredSet := map[int64]bool{}
	for _, id := range covered {
		coveredSet[id] = true
	}
	carriedIDs, err := d.resolveCarriedRowIDs(ctx, conn, sessionID, o.CarriedMessages)
	if err != nil {
		return 0, err
	}
	for _, id := range carriedIDs {
		coveredSet[id] = true
	}
	var unseen, coveredActive []int64
	for _, id := range activeIDs {
		if coveredSet[id] {
			coveredActive = append(coveredActive, id)
		} else {
			unseen = append(unseen, id)
		}
	}
	carriedUnseen, weakClaims, err := d.carriedTailIDs(ctx, conn, unseen, compacted)
	if err != nil {
		return 0, err
	}
	var cloneIDs []int64
	for _, id := range unseen {
		if _, carried := carriedUnseen[id]; !carried {
			cloneIDs = append(cloneIDs, id)
		}
	}
	tailBelow := o.TailCount - carriedTailSlots(carriedUnseen, len(compacted), o.TailCount)
	rewindIDs := append([]int64{}, carriedIDs...)
	if tailBelow > 0 {
		rewindIDs = append(rewindIDs, tailOriginals(coveredActive, tailBelow, mergedAway)...)
	}
	for _, id := range unseen {
		if !weakClaims[id] {
			rewindIDs = append(rewindIDs, id)
		}
	}
	rewindIDs = uniqueIDs(rewindIDs)
	if len(rewindIDs) > 0 {
		if err := rewindRows(ctx, conn, sessionID, rewindIDs); err != nil {
			return 0, err
		}
	}
	if _, err := conn.ExecContext(ctx, archiveActiveSQL, sessionID); err != nil {
		return 0, err
	}
	d.carryParentTimestamps(ctx, conn, sessionID, compacted)
	inserted, toolCallsTotal, err := d.insertMessageRows(ctx, conn, sessionID, compacted, true)
	if err != nil {
		return 0, err
	}
	if len(cloneIDs) > 0 {
		_, unseenToolCalls, err := tailRowsAfterWatermark(ctx, conn, "SELECT id, tool_calls FROM messages WHERE id IN ("+placeholders(len(cloneIDs))+") ORDER BY id", int64sToAny(cloneIDs)...)
		if err != nil {
			return 0, err
		}
		if err := d.cloneMessageRows(ctx, conn, cloneIDs, nil); err != nil {
			return 0, err
		}
		inserted += len(cloneIDs)
		toolCallsTotal += unseenToolCalls
	}
	if err := d.reconcileDisplayOrders(ctx, conn, sessionID); err != nil {
		return 0, err
	}
	return inserted, d.setCompactedCounters(ctx, conn, sessionID, inserted, toolCallsTotal, patched, patch)
}

// Soft archives active rows and inserts the compacted set as fresh active rows atomically, returning the active count
func (d *SessionDB) ArchiveAndCompact(ctx context.Context, sessionID string, compacted []map[string]any, o ArchiveCompactOptions) (int, error) {
	var inserted int
	err := d.executeTranscriptWrite(ctx, func(conn SQLiteConn) error {
		var err error
		inserted, err = d.archiveAndCompactOnConn(ctx, conn, sessionID, compacted, o)
		return err
	}, compacted, nil)
	return inserted, err
}

// In place compaction on the caller's write transaction, shared by the owner and the worker verbs
func (d *SessionDB) archiveAndCompactOnConn(ctx context.Context, conn SQLiteConn, sessionID string, compacted []map[string]any, o ArchiveCompactOptions) (int, error) {
	if o.LockHolder != nil {
		lock, err := connRow(ctx, conn, compressionLockRowSQL, sessionID)
		if err != nil {
			return 0, err
		}
		lost := lock == nil || lock["holder"] != *o.LockHolder
		if !lost {
			expires, err := pyFloatOf(lock["expires_at"])
			if err != nil {
				return 0, err
			}
			lost = expires <= now()
		}
		if lost {
			return 0, &SessionCompressionInProgressError{Msg: "Compression lease for " + pyRepr(sessionID) + " lost before commit; refusing to publish a stale compaction"}
		}
	}
	patch := o.ModelConfigPatch != nil
	var patched any
	if patch {
		value, _, err := mergeModelConfigJSON(ctx, conn, sessionID, o.ModelConfigPatch, true)
		if err != nil {
			return 0, err
		}
		patched = value
	}
	if len(o.HeldForCAS) > 0 {
		mismatches, err := d.heldRowMismatches(ctx, conn, sessionID, o.HeldForCAS)
		if err != nil {
			return 0, err
		}
		if len(mismatches) > 0 {
			return 0, &HeldRowRewrittenError{Msg: "a held row of session " + sessionID + " was rewritten in place before the commit"}
		}
	}
	proved, mergedAway, ok, err := provedCoverage(ctx, conn, sessionID, o.CoveredIDs, o.UnresolvedHeld, o.Watermark)
	if err != nil {
		return 0, err
	}
	if ok {
		return d.archiveNamedRows(ctx, conn, sessionID, compacted, proved, o, patched, patch, mergedAway)
	}
	var tailIDs []int64
	if o.Watermark != nil {
		rows, err := connRows(ctx, conn, "SELECT id FROM messages WHERE session_id = ? AND active = 1 AND id > ? ORDER BY id", sessionID, *o.Watermark)
		if err != nil {
			return 0, err
		}
		tailIDs = rowIDList(rows)
	}
	carriedTail, weakClaims, err := d.carriedTailIDs(ctx, conn, tailIDs, compacted)
	if err != nil {
		return 0, err
	}
	var cloneIDs []int64
	for _, id := range tailIDs {
		if _, carried := carriedTail[id]; !carried {
			cloneIDs = append(cloneIDs, id)
		}
	}
	tailToolCalls := 0
	if len(cloneIDs) > 0 {
		if _, tailToolCalls, err = tailRowsAfterWatermark(ctx, conn, "SELECT id, tool_calls FROM messages WHERE id IN ("+placeholders(len(cloneIDs))+") ORDER BY id", int64sToAny(cloneIDs)...); err != nil {
			return 0, err
		}
	}
	tailBelow := o.TailCount - carriedTailSlots(carriedTail, len(compacted), o.TailCount)
	rewindIDs, err := d.resolveCarriedRowIDs(ctx, conn, sessionID, o.CarriedMessages)
	if err != nil {
		return 0, err
	}
	if tailBelow > 0 {
		uncounted, err := uncountedMergedRows(pyTail(compacted, o.TailCount))
		if err != nil {
			return 0, err
		}
		query, args := "SELECT id FROM messages WHERE session_id = ? AND active = 1", []any{sessionID}
		if o.Watermark != nil {
			query += " AND id <= ?"
			args = append(args, *o.Watermark)
		}
		rows, err := connRows(ctx, conn, query+" ORDER BY id DESC LIMIT ?", append(args, tailBelow+uncounted)...)
		if err != nil {
			return 0, err
		}
		rewindIDs = append(rewindIDs, rowIDList(rows)...)
	}
	for _, id := range tailIDs {
		if !weakClaims[id] {
			rewindIDs = append(rewindIDs, id)
		}
	}
	rewindIDs = uniqueIDs(rewindIDs)
	if len(rewindIDs) > 0 {
		if err := rewindRows(ctx, conn, sessionID, rewindIDs); err != nil {
			return 0, err
		}
		if _, err := conn.ExecContext(ctx, archiveActiveSQL+" AND id NOT IN ("+placeholders(len(rewindIDs))+")", append([]any{sessionID}, int64sToAny(rewindIDs)...)...); err != nil {
			return 0, err
		}
	} else if _, err := conn.ExecContext(ctx, archiveActiveSQL, sessionID); err != nil {
		return 0, err
	}
	d.carryParentTimestamps(ctx, conn, sessionID, compacted)
	inserted, toolCallsTotal, err := d.insertMessageRows(ctx, conn, sessionID, compacted, true)
	if err != nil {
		return 0, err
	}
	if len(cloneIDs) > 0 {
		if err := d.cloneMessageRows(ctx, conn, cloneIDs, nil); err != nil {
			return 0, err
		}
		inserted += len(cloneIDs)
		toolCallsTotal += tailToolCalls
	}
	if err := d.reconcileDisplayOrders(ctx, conn, sessionID); err != nil {
		return 0, err
	}
	return inserted, d.setCompactedCounters(ctx, conn, sessionID, inserted, toolCallsTotal, patched, patch)
}
