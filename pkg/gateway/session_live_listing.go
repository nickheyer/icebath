package gateway

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Sessions whose store facts one worker read fetches
const listingBatch = 256

// Sources of worker sessions every conversation listing leaves out
var InternalListingSources = []string{"kanban", "tool", "oneshot"}

var listingSkippedReasons = map[string]bool{"permission_denied": true, "profile_mismatch": true, "not_found": true}

// One live session an actor may read with its handle, picker row, pending admissions and closed verdict
type liveEntry struct {
	sessionID string
	live      *LiveSession
	handle    SessionHandle
	row       map[string]any
	pending   []*pyjson.Object
	closed    bool
}

// Rows of a query as column keyed maps
func scanListingRows(rows *sql.Rows) ([]map[string]any, error) {
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(columns))
		for i, name := range columns {
			if b, ok := values[i].([]byte); ok {
				values[i] = string(b)
			}
			row[name] = values[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Picker rows and pending admissions of a batch of sessions in one read
func listingStoreFacts(ctx context.Context, db *state.SessionDB, sessionIDs []string) (map[string]map[string]any, map[string][]*pyjson.Object, error) {
	idList := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		idList[i] = id
	}
	ids, err := pyjson.Dumps(idList)
	if err != nil {
		return nil, nil, err
	}
	var rows, pending []map[string]any
	err = db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		result, err := conn.QueryContext(ctx,
			"SELECT s.id, s.source, s.title, s.started_at, s.ended_at, s.message_count, s.model, s.hidden, "+
				"s.archived, s.runtime_revision, s.runtime_generation, "+
				"(SELECT t.ended_at FROM state_meta m JOIN sessions t ON t.id = CASE WHEN json_valid(m.value) "+
				"THEN json_extract(m.value, '$.entry.session_id') END WHERE m.key = ? || s.id) AS target_ended_at "+
				"FROM sessions s WHERE s.id IN (SELECT value FROM json_each(?))", state.LocalPolicyPrefix, ids)
		if err != nil {
			return err
		}
		if rows, err = scanListingRows(result); err != nil {
			return err
		}
		result, err = conn.QueryContext(ctx,
			"SELECT target_session_id, admission_id, seq, status FROM session_admissions "+
				"WHERE target_session_id IN (SELECT value FROM json_each(?)) "+
				"AND status IN ('queued','started','unknown') ORDER BY seq", ids)
		if err != nil {
			return err
		}
		pending, err = scanListingRows(result)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		byID[pyrepr.Str(row["id"])] = row
	}
	bySession := map[string][]*pyjson.Object{}
	for _, row := range pending {
		target := pyrepr.Str(row["target_session_id"])
		bySession[target] = append(bySession[target], pyjson.ObjectOf(
			"target_session_id", row["target_session_id"], "admission_id", row["admission_id"], "seq", row["seq"], "status", row["status"]))
	}
	return byID, bySession, nil
}

// Pending rows with an unpublished settlement still reading as its started claim
func maskedPending(live *LiveSession, rows []*pyjson.Object) []*pyjson.Object {
	if len(live.Settling) == 0 {
		return rows
	}
	merged := map[string]*pyjson.Object{}
	var order []string
	put := func(id string, row *pyjson.Object) {
		if _, seen := merged[id]; !seen {
			order = append(order, id)
		}
		merged[id] = row
	}
	for _, row := range rows {
		put(pyrepr.Str(row.GetOr("admission_id", nil)), row)
	}
	for id, row := range live.Settling {
		put(id, row)
	}
	out := make([]*pyjson.Object, 0, len(order))
	for _, id := range order {
		out = append(out, merged[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return anyInt64(out[i].GetOr("seq", nil)) < anyInt64(out[j].GetOr("seq", nil)) })
	return out
}

// Execution state the pending rows imply, unknown over running over idle
func listingExecutionState(pending []*pyjson.Object) string {
	statuses := map[any]bool{}
	for _, row := range pending {
		statuses[row.GetOr("status", nil)] = true
	}
	switch {
	case statuses["unknown"]:
		return "unknown"
	case statuses["started"]:
		return "running"
	}
	return "idle"
}

// Whether the actor may read a session still in memory, a skipped refusal reading as false
func listingReadable(ctx context.Context, a *SessionAuthority, actor Principal, sessionID string) (bool, error) {
	if _, ok := a.live(sessionID); !ok {
		return false, nil
	}
	err := a.Authorize(WithListingRead(ctx), actor, SessionRef{ProfileID: actor.ProfileID, SessionID: sessionID}, "session:read")
	if err == nil {
		return true, nil
	}
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) && listingSkippedReasons[refusal.Reason] {
		return false, nil
	}
	return false, err
}

// Store facts of one listing batch
type listingFacts struct {
	rows    map[string]map[string]any
	pending map[string][]*pyjson.Object
}

// Closed idle and unviewed verdict on facts already read
func closedIdleUnviewed(live *LiveSession, executionState string, targetEndedAt any) bool {
	if live == nil || len(live.Subscribers) > 0 || executionState != "idle" {
		return false
	}
	if liveDetached(live) || isHistoryMount(live) {
		return true
	}
	return targetEndedAt != nil
}

// Every live session the actor may read with its handle, picker row and closed verdict
func liveEntries(ctx context.Context, a *SessionAuthority, actor Principal) ([]liveEntry, error) {
	a.mu.Lock()
	snapshot := make([]string, 0, len(a.Sessions))
	for id := range a.Sessions {
		snapshot = append(snapshot, id)
	}
	a.mu.Unlock()
	sort.Strings(snapshot)
	var entries []liveEntry
	for start := 0; start < len(snapshot); start += listingBatch {
		end := min(start+listingBatch, len(snapshot))
		var batch []string
		for _, id := range snapshot[start:end] {
			readable, err := listingReadable(ctx, a, actor, id)
			if err != nil {
				return nil, err
			}
			if readable {
				batch = append(batch, id)
			}
		}
		if len(batch) == 0 {
			continue
		}
		facts, err := forwardOffThread(ctx, func() (listingFacts, error) {
			rows, pending, err := listingStoreFacts(ctx, a.DB, batch)
			return listingFacts{rows, pending}, err
		})
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		gone := a.Retiring || a.Retired
		a.mu.Unlock()
		if gone {
			return []liveEntry{}, nil
		}
		for _, id := range batch {
			live, held := a.live(id)
			row, found := facts.rows[id]
			if !held || !found {
				continue
			}
			masked := maskedPending(live, facts.pending[id])
			executionState := listingExecutionState(masked)
			handle := SessionHandle{Ref: SessionRef{ProfileID: a.ProfileID, SessionID: id}, InstanceID: a.InstanceID,
				AuthorityEpoch: a.Epoch, Revision: anyInt64(row["runtime_revision"]),
				ExecutionGeneration: anyInt64(row["runtime_generation"]), ExecutionState: executionState}
			entries = append(entries, liveEntry{sessionID: id, live: live, handle: handle, row: row, pending: masked,
				closed: closedIdleUnviewed(live, executionState, row["target_ended_at"])})
		}
	}
	return entries, nil
}

// Integer column value, zero for NULL
func anyInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
