package state

import (
	"context"
	"errors"
	"sort"
)

// True for a live turn lease or compression lock refusal, the retryable busy verdict of a running admission
func IsMutationGuardRefusal(err error) bool {
	var lease *SessionTurnLeaseLostError
	var compressing *SessionCompressionInProgressError
	return errors.As(err, &lease) || errors.As(err, &compressing)
}

// Client facing busy refusal for a guard refusal, naming a held compression lock in its field
func BusyRefusal(err error) *RuntimeStoreError {
	var compressing *SessionCompressionInProgressError
	if errors.As(err, &compressing) {
		field := "compression"
		return NewRuntimeStoreError("session_busy", &field, nil)
	}
	return NewRuntimeStoreError("session_busy", nil, nil)
}

// Live admission and worker statuses of one session among the given admission states
func executionStates(ctx context.Context, conn SQLiteConn, sessionID, admissionStates string) (map[string]bool, error) {
	admissions, err := queryStrings(ctx, conn, "SELECT status FROM session_admissions WHERE target_session_id=? AND status IN ("+admissionStates+")", sessionID)
	if err != nil {
		return nil, err
	}
	workers, err := queryStrings(ctx, conn, "SELECT status FROM worker_executions WHERE session_id=? AND status IN ('registered','running','unknown')", sessionID)
	if err != nil {
		return nil, err
	}
	states := map[string]bool{}
	for _, s := range append(admissions, workers...) {
		states[s] = true
	}
	return states, nil
}

// Refusal for live states, unknown execution winning over busy
func executionRefusal(states map[string]bool) error {
	if states["unknown"] {
		return runtimeRefusal("unknown_execution")
	}
	if len(states) > 0 {
		return runtimeRefusal("session_busy")
	}
	return nil
}

// Refuses while a turn or worker runs or has an unknown outcome on any session, queued followers allowed
func RequireNotExecuting(ctx context.Context, conn SQLiteConn, sessionIDs []string) error {
	for _, sid := range sessionIDs {
		states, err := executionStates(ctx, conn, sid, "'started','unknown'")
		if err != nil {
			return err
		}
		if err := executionRefusal(states); err != nil {
			return err
		}
	}
	return nil
}

// Refuses a live turn lease or compression lock on a transcript, skipping an ancestor closed by compression
func (d *SessionDB) requireTranscriptUnleased(ctx context.Context, conn SQLiteConn, sessionID string) error {
	ended, err := connRow(ctx, conn, endedRowSQL, sessionID)
	if err != nil {
		return err
	}
	if endedByCompression(ended) {
		return nil
	}
	return d.checkTranscriptWriteGuards(ctx, conn, sessionID, transcriptGuards{rejectActiveTurnLease: true, rejectActiveCompressionLock: true})
}

// Fence for writes that publish a new physical target, refusing executing work and live transcript leases
func (d *SessionDB) RequireTargetAdvanceable(ctx context.Context, conn SQLiteConn, sessionIDs []string) error {
	if err := RequireNotExecuting(ctx, conn, sessionIDs); err != nil {
		return err
	}
	for _, sid := range sessionIDs {
		if err := d.requireTranscriptUnleased(ctx, conn, sid); err != nil {
			return err
		}
	}
	return nil
}

// Refuses any queued, started or unknown admission, any live worker and any live transcript lease
func (d *SessionDB) RequireIdle(ctx context.Context, conn SQLiteConn, sessionIDs []string) error {
	for _, sid := range sessionIDs {
		states, err := executionStates(ctx, conn, sid, "'queued','started','unknown'")
		if err != nil {
			return err
		}
		if err := executionRefusal(states); err != nil {
			return err
		}
		if err := d.requireTranscriptUnleased(ctx, conn, sid); err != nil {
			return err
		}
	}
	return nil
}

// Continuation children and compression parent one step from the session, fork markers matched against the edge parent
func continuationEdges(ctx context.Context, conn SQLiteConn, sid string) ([]string, error) {
	edge := nonContinuationChildSQL("child.", "parent.id")
	rows, err := connRows(ctx, conn, "SELECT child.id FROM sessions parent JOIN sessions child ON child.parent_session_id=parent.id "+
		"WHERE parent.id=? AND parent.end_reason='compression'\n"+edge+
		" UNION SELECT parent.id FROM sessions child JOIN sessions parent ON parent.id=child.parent_session_id "+
		"WHERE child.id=? AND parent.end_reason='compression'\n"+edge, sid, sid)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, pyStr(row["id"]))
	}
	return ids, nil
}

// Requested session first, then every lineage segment, continuation and delegate the delete takes, sorted
func DeleteTargets(ctx context.Context, conn SQLiteConn, sessionID string) ([]string, error) {
	targets := map[string]bool{sessionID: true}
	owner, err := localLineageOwner(ctx, conn, sessionID)
	if err != nil {
		return nil, err
	}
	targets[owner] = true
	raw, ok, err := fetchScalar(ctx, conn, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+owner)
	if err != nil {
		return nil, err
	}
	if ok {
		receipt, err := loadsStoredObject(raw)
		if err != nil {
			return nil, err
		}
		if _, err := validateLocalLineage(ctx, conn, receipt); err != nil {
			return nil, err
		}
		lineage, has := receipt.Get("lineage")
		if !has {
			lineage = []any{sessionID}
		}
		items, isList := lineage.([]any)
		if !isList {
			return nil, &stateTypedError{"TypeError", "'" + pyTypeName(lineage) + "' object is not iterable"}
		}
		for _, item := range items {
			sid, isStr := item.(string)
			if !isStr {
				return nil, &stateTypedError{"TypeError", "'<' not supported between instances of '" + pyTypeName(item) + "' and 'str'"}
			}
			targets[sid] = true
		}
	}
	anchor := map[string]string{}
	frontier := make([]string, 0, len(targets))
	for sid := range targets {
		anchor[sid] = sessionID
		frontier = append(frontier, sid)
	}
	sort.Strings(frontier)
	for range chainCap {
		found := map[string]string{}
		var order []string
		for _, sid := range frontier {
			edges, err := continuationEdges(ctx, conn, sid)
			if err != nil {
				return nil, err
			}
			for _, next := range edges {
				if targets[next] {
					continue
				}
				if _, seen := found[next]; seen {
					continue
				}
				same, err := SameHistoryOwner(ctx, conn, anchor[sid], next)
				if err != nil {
					return nil, err
				}
				if same {
					found[next] = anchor[sid]
					order = append(order, next)
				}
			}
		}
		delegates, err := collectDelegateChildIDs(ctx, conn, frontier)
		if err != nil {
			return nil, err
		}
		for _, sid := range delegates {
			if !targets[sid] {
				if _, seen := found[sid]; !seen {
					found[sid] = sid
					order = append(order, sid)
				}
			}
		}
		if len(found) == 0 {
			break
		}
		for sid, root := range found {
			anchor[sid] = root
			targets[sid] = true
		}
		frontier = order
	}
	rest := make([]string, 0, len(targets))
	for sid := range targets {
		if sid != sessionID {
			rest = append(rest, sid)
		}
	}
	sort.Strings(rest)
	return append([]string{sessionID}, rest...), nil
}
