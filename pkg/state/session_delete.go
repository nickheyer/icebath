package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Ids under a live turn lease or compression lock, idle compression ended parents counting as closed
func (d *SessionDB) guardedIDs(ctx context.Context, conn SQLiteConn, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, sid := range ids {
		rejected, err := d.writeGuardsReject(ctx, conn, sid, transcriptGuards{allowClosedCompressionParent: true})
		if err != nil {
			return nil, err
		}
		if rejected {
			out[sid] = true
		}
	}
	return out, nil
}

// True when a live turn lease or compression lock protects the session, reclaiming stale guards on the way
func (d *SessionDB) writeGuardsReject(ctx context.Context, conn SQLiteConn, sid string, g transcriptGuards) (bool, error) {
	g.compressionLockHolder, g.turnLeaseHolder = nil, nil
	g.rejectActiveTurnLease, g.rejectActiveCompressionLock = true, true
	err := d.checkTranscriptWriteGuards(ctx, conn, sid, g)
	var busy *SessionCompressionInProgressError
	var lost *SessionTurnLeaseLostError
	if errors.As(err, &busy) || errors.As(err, &lost) {
		return true, nil
	}
	return false, err
}

// One both direction compression edge step from the frontier over the list projection edge definition
func expandCompressionLineageIDs(ctx context.Context, conn SQLiteConn, frontier []string) ([]string, error) {
	var ids []string
	for _, sid := range frontier {
		if sid != "" {
			ids = append(ids, sid)
		}
	}
	childFilter := sqlJSONExtract("child.model_config", "$._branched_from") + " IS NULL " +
		"AND " + sqlJSONExtract("child.model_config", "$._delegate_from") + " IS NULL " +
		"AND NOT (" + resetChildSQL("child") + ") " +
		"AND COALESCE(child.source, '') != 'tool'"
	var out []string
	for _, chunk := range idChunks(ids, sqlInChunk/4) {
		ph := placeholders(len(chunk))
		forward, err := queryStrings(ctx, conn, "SELECT child.id FROM sessions parent "+
			"JOIN sessions child ON child.parent_session_id = parent.id "+
			"WHERE parent.id IN ("+ph+") AND parent.end_reason = 'compression' AND "+childFilter, stringsToAny(chunk)...)
		if err != nil {
			return nil, err
		}
		backward, err := queryStrings(ctx, conn, "SELECT parent.id FROM sessions child "+
			"JOIN sessions parent ON parent.id = child.parent_session_id "+
			"WHERE child.id IN ("+ph+") AND parent.end_reason = 'compression' "+
			"AND "+childFilter, stringsToAny(chunk)...)
		if err != nil {
			return nil, err
		}
		out = append(append(out, forward...), backward...)
	}
	return out, nil
}

// Delegate subagent ids walked recursively from the parents, never returning a parent
func collectDelegateChildIDs(ctx context.Context, conn SQLiteConn, parentIDs []string) ([]string, error) {
	df := sqlJSONExtract("model_config", "$._delegate_from")
	seeds := map[string]bool{}
	var frontier []string
	for _, sid := range parentIDs {
		if sid != "" && !seeds[sid] {
			seeds[sid] = true
			frontier = append(frontier, sid)
		}
	}
	found := map[string]bool{}
	for k := range seeds {
		found[k] = true
	}
	var out []string
	for len(frontier) > 0 {
		var next []string
		for _, chunk := range idChunks(frontier, sqlInChunk/2) {
			ph := placeholders(len(chunk))
			rows, err := queryStrings(ctx, conn, "SELECT id FROM sessions WHERE "+df+" IN ("+ph+") "+
				"OR (parent_session_id IN ("+ph+") AND "+df+" IS NOT NULL)", append(stringsToAny(chunk), stringsToAny(chunk)...)...)
			if err != nil {
				return nil, err
			}
			for _, id := range rows {
				if !found[id] {
					found[id] = true
					next = append(next, id)
					out = append(out, id)
				}
			}
		}
		frontier = next
	}
	return out, nil
}

// Retires and deletes the delegate children of the parents, returning their ids
func deleteDelegateChildren(ctx context.Context, conn SQLiteConn, parentIDs []string) ([]string, error) {
	ids, err := collectDelegateChildIDs(ctx, conn, parentIDs)
	if err != nil {
		return nil, err
	}
	if err := retireSessions(ctx, conn, ids, nil); err != nil {
		return nil, err
	}
	if err := deleteSessionRows(ctx, conn, ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// Deletes the messages and rows of the ids after orphaning children that point at them
func deleteSessionRows(ctx context.Context, conn SQLiteConn, ids []string) error {
	for _, chunk := range idChunks(ids, sqlInChunk) {
		ph := placeholders(len(chunk))
		for _, q := range []string{
			"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IN (" + ph + ")",
			"DELETE FROM messages WHERE session_id IN (" + ph + ")",
			"DELETE FROM sessions WHERE id IN (" + ph + ")",
		} {
			if _, err := conn.ExecContext(ctx, q, stringsToAny(chunk)...); err != nil {
				return err
			}
		}
	}
	return nil
}

// Glob pattern matching a session id literally
func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '*' || r == '?' || r == '[' {
			b.WriteString("[" + string(r) + "]")
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Removes every historical transcript snapshot and request dump of a session, swallowing filesystem errors
func removeSessionFiles(sessionsDir, sessionID string) {
	if sessionsDir == "" {
		return
	}
	targets := []string{filepath.Join(sessionsDir, sessionID+".json"), filepath.Join(sessionsDir, sessionID+".jsonl"), filepath.Join(sessionsDir, "session_"+sessionID+".json")}
	if dumps, err := filepath.Glob(filepath.Join(sessionsDir, "request_dump_"+globEscape(sessionID)+"_*.json")); err == nil {
		targets = append(targets, dumps...)
	}
	for _, p := range targets {
		_ = os.Remove(p)
	}
}

// Rows a delete of the session would remove, the session then its lineage then its delegate children
func (d *SessionDB) GetSessionDeleteTargets(ctx context.Context, sessionID string) ([]string, error) {
	var out []string
	err := d.readCtx(ctx, false, func(conn SQLiteConn) error {
		exists, err := connRow(ctx, conn, "SELECT 1 AS one FROM sessions WHERE id = ? LIMIT 1", sessionID)
		if err != nil || exists == nil {
			out = []string{}
			return err
		}
		scope, err := ownedLineageIDs(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		delegates, err := collectDelegateChildIDs(ctx, conn, scope)
		if err != nil {
			return err
		}
		rest := slices.DeleteFunc(slices.Clone(scope), func(s string) bool { return s == sessionID })
		slices.Sort(rest)
		rest = slices.Compact(rest)
		slices.Sort(delegates)
		out = append(append([]string{sessionID}, rest...), delegates...)
		return nil
	})
	return out, err
}

// Expands the ids to every member of their compression chains and local conversations
func (d *SessionDB) expandCompressionLineage(ctx context.Context, conn SQLiteConn, sessionIDs []string) ([]string, error) {
	seen := orderedIDs{}
	seen.add(sessionIDs...)
	frontier := slices.Clone(sessionIDs)
	for range chainCap {
		if len(frontier) == 0 {
			break
		}
		targets, err := expandCompressionLineageIDs(ctx, conn, frontier)
		if err != nil {
			return nil, err
		}
		frontier = frontier[:0:0]
		for _, sid := range targets {
			if !seen.seen[sid] {
				frontier = append(frontier, sid)
			}
		}
		seen.add(frontier...)
	}
	for _, sid := range slices.Clone(seen.list) {
		ids, err := localConversationIDs(ctx, conn, sid)
		if err != nil {
			return nil, err
		}
		seen.add(ids...)
	}
	return seen.list, nil
}

// Delete options, nil ExpectedDeleteIDs and ExpectedDisplayMessages meaning no fence
type DeleteSessionOptions struct {
	SessionsDir              string
	ExpectedDeleteIDs        []string
	ExpectedDisplayMessages  map[string][]*pyjson.Object
	ExcludeActiveWriteGuards bool
	IncludeCompressionChain  bool
}

// Deletes a session with its lineage and delegate children inside drift fences, orphaning branch children
func (d *SessionDB) DeleteSession(ctx context.Context, sessionID string, o DeleteSessionOptions) (bool, error) {
	if o.IncludeCompressionChain {
		n, err := d.DeleteSessions(ctx, []string{sessionID}, DeleteSessionsOptions{SessionsDir: o.SessionsDir, ExcludeActiveWriteGuards: o.ExcludeActiveWriteGuards, IncludeCompressionChain: true})
		return n > 0, err
	}
	var removed []string
	deleted := false
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		removed, deleted = nil, false
		exists, err := connRow(ctx, conn, "SELECT 1 AS one FROM sessions WHERE id = ? LIMIT 1", sessionID)
		if err != nil || exists == nil {
			return err
		}
		scope, err := ownedLineageIDs(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		var targets []string
		if o.ExcludeActiveWriteGuards || o.ExpectedDeleteIDs != nil {
			delegates, err := collectDelegateChildIDs(ctx, conn, scope)
			if err != nil {
				return err
			}
			targets = append(slices.Clone(scope), delegates...)
		}
		if o.ExcludeActiveWriteGuards {
			guarded, err := d.guardedIDs(ctx, conn, targets)
			if err != nil {
				return err
			}
			if len(guarded) > 0 {
				return &SessionActiveWriteGuardError{Msg: "session '" + sessionID + "' (or a delegate child) has an active turn lease or compression lock"}
			}
		}
		if o.ExpectedDeleteIDs != nil && !sameIDSet(o.ExpectedDeleteIDs, targets) {
			return nil
		}
		for covered, expected := range o.ExpectedDisplayMessages {
			got, ok, err := d.displayMessagesFromConn(ctx, conn, covered)
			if err != nil {
				return err
			}
			if !ok || !displayMessagesEqual(got, expected) {
				return nil
			}
		}
		if err := retireSessions(ctx, conn, scope, nil); err != nil {
			return err
		}
		children, err := deleteDelegateChildren(ctx, conn, scope)
		if err != nil {
			return err
		}
		removed = append(removed, children...)
		for _, sid := range scope {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id = ?", sid); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sid); err != nil {
				return err
			}
		}
		for _, sid := range scope {
			if _, err := conn.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", sid); err != nil {
				return err
			}
		}
		if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
			return err
		}
		removed = append(removed, scope...)
		deleted = true
		return nil
	}, nil)
	if err != nil {
		return false, err
	}
	if deleted {
		d.EmitSessionDeleted(ctx, removed, "delete", sessionID, o.SessionsDir)
	}
	if err := d.CollectRetiredMedia(ctx); err != nil {
		return false, err
	}
	for _, sid := range removed {
		removeSessionFiles(o.SessionsDir, sid)
	}
	return deleted, nil
}

// Same members regardless of order and duplicates
func sameIDSet(a, b []string) bool {
	x, y := map[string]bool{}, map[string]bool{}
	for _, s := range a {
		x[s] = true
	}
	for _, s := range b {
		y[s] = true
	}
	if len(x) != len(y) {
		return false
	}
	for s := range x {
		if !y[s] {
			return false
		}
	}
	return true
}

// Python list equality of two display message lists
func displayMessagesEqual(a, b []*pyjson.Object) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !pyjson.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Removes a just created row nobody was admitted against without the retirement fence, else the fenced delete
func (d *SessionDB) DiscardUnadmittedSession(ctx context.Context, sessionID string) (bool, error) {
	var result *bool
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		result = nil
		admitted, err := connRow(ctx, conn, "SELECT 1 AS one FROM session_admissions WHERE target_session_id=? UNION ALL "+
			"SELECT 1 FROM worker_executions WHERE session_id=? LIMIT 1", sessionID, sessionID)
		if err != nil || admitted != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", sessionID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		gone := n > 0
		result = &gone
		return nil
	}, nil)
	if err != nil {
		return false, err
	}
	if result == nil {
		return d.DeleteSession(ctx, sessionID, DeleteSessionOptions{})
	}
	return *result, nil
}

// Deletes an untitled session with no messages and no children unless a live write guard protects it
func (d *SessionDB) DeleteSessionIfEmpty(ctx context.Context, sessionID, sessionsDir string) (bool, error) {
	deleted := false
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		deleted = false
		guarded, err := d.guardedIDs(ctx, conn, []string{sessionID})
		if err != nil || len(guarded) > 0 {
			return err
		}
		owned, err := ownedLineageIDs(ctx, conn, sessionID)
		if err != nil || len(owned) != 1 || owned[0] != sessionID {
			return err
		}
		eligible, err := connRow(ctx, conn, `
                SELECT 1 AS one FROM sessions
                WHERE id = ?
                  AND title IS NULL
                  AND NOT EXISTS (
                      SELECT 1 FROM messages WHERE messages.session_id = sessions.id
                  )
                  AND NOT EXISTS (
                      SELECT 1 FROM sessions child
                      WHERE child.parent_session_id = sessions.id
                  )
                `, sessionID)
		if err != nil || eligible == nil {
			return err
		}
		if err := retireSessions(ctx, conn, []string{sessionID}, []string{sessionID}); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", sessionID); err != nil {
			return err
		}
		if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
			return err
		}
		deleted = true
		return nil
	}, nil)
	if err != nil {
		return false, err
	}
	if deleted {
		d.EmitSessionDeleted(ctx, []string{sessionID}, "empty_sweep", "", sessionsDir)
	}
	if err := d.CollectRetiredMedia(ctx); err != nil {
		return false, err
	}
	if deleted {
		removeSessionFiles(sessionsDir, sessionID)
	}
	return deleted, nil
}

// Bulk delete options, SkippedIDs collecting the guarded ids a guarded delete skipped
type DeleteSessionsOptions struct {
	SessionsDir              string
	ExcludeActiveWriteGuards bool
	SkippedIDs               *[]string
	IncludeCompressionChain  bool
}

// Deletes the sessions with delete_session semantics in one transaction, returning how many requested rows went
func (d *SessionDB) DeleteSessions(ctx context.Context, sessionIDs []string, o DeleteSessionsOptions) (int, error) {
	unique := orderedIDs{}
	for _, sid := range sessionIDs {
		if sid != "" {
			unique.add(sid)
		}
	}
	if len(unique.list) == 0 {
		return 0, nil
	}
	var removed []string
	count := 0
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		removed, count = nil, 0
		var existing []string
		for _, chunk := range idChunks(unique.list, sqlInChunk) {
			rows, err := queryStrings(ctx, conn, "SELECT id FROM sessions WHERE id IN ("+placeholders(len(chunk))+")", stringsToAny(chunk)...)
			if err != nil {
				return err
			}
			existing = append(existing, rows...)
		}
		if len(existing) == 0 {
			return nil
		}
		expand := func(ids []string) ([]string, error) {
			if o.IncludeCompressionChain {
				return d.expandCompressionLineage(ctx, conn, ids)
			}
			out := orderedIDs{}
			for _, sid := range ids {
				owned, err := ownedLineageIDs(ctx, conn, sid)
				if err != nil {
					return nil, err
				}
				out.add(owned...)
			}
			return out.list, nil
		}
		withDelegates := func(ids []string) ([]string, error) {
			delegates, err := collectDelegateChildIDs(ctx, conn, ids)
			if err != nil {
				return nil, err
			}
			return append(slices.Clone(ids), delegates...), nil
		}
		expanded, err := expand(existing)
		if err != nil {
			return err
		}
		if o.ExcludeActiveWriteGuards {
			pool, err := withDelegates(expanded)
			if err != nil {
				return err
			}
			anyGuarded, err := d.guardedIDs(ctx, conn, pool)
			if err != nil {
				return err
			}
			active := map[string]bool{}
			if len(anyGuarded) > 0 {
				for _, sid := range existing {
					var members []string
					if o.IncludeCompressionChain {
						lineage, err := d.expandCompressionLineage(ctx, conn, []string{sid})
						if err != nil {
							return err
						}
						set := orderedIDs{}
						set.add(lineage...)
						set.add(sid)
						members = set.list
					} else if members, err = expand([]string{sid}); err != nil {
						return err
					}
					pool, err := withDelegates(members)
					if err != nil {
						return err
					}
					guarded, err := d.guardedIDs(ctx, conn, pool)
					if err != nil {
						return err
					}
					if len(guarded) > 0 {
						active[sid] = true
					}
				}
			}
			existing = slices.DeleteFunc(existing, func(s string) bool { return active[s] })
			if o.SkippedIDs != nil {
				skipped := make([]string, 0, len(active))
				for s := range active {
					skipped = append(skipped, s)
				}
				slices.Sort(skipped)
				*o.SkippedIDs = append(*o.SkippedIDs, skipped...)
			}
			if len(existing) == 0 {
				return nil
			}
			if expanded, err = expand(existing); err != nil {
				return err
			}
		}
		if err := retireSessions(ctx, conn, expanded, nil); err != nil {
			return err
		}
		children, err := deleteDelegateChildren(ctx, conn, expanded)
		if err != nil {
			return err
		}
		removed = append(removed, children...)
		if err := deleteSessionRows(ctx, conn, expanded); err != nil {
			return err
		}
		if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
			return err
		}
		removed = append(removed, expanded...)
		count = len(existing)
		return nil
	}, nil)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		primary := ""
		if len(unique.list) == 1 {
			primary = unique.list[0]
		}
		d.EmitSessionDeleted(ctx, removed, "delete", primary, o.SessionsDir)
	}
	if err := d.CollectRetiredMedia(ctx); err != nil {
		return 0, err
	}
	for _, sid := range removed {
		removeSessionFiles(o.SessionsDir, sid)
	}
	return count, nil
}

// Count of empty ended unarchived sessions
func (d *SessionDB) CountEmptySessions(ctx context.Context) (int, error) {
	row, err := d.readOne(ctx, "SELECT COUNT(*) AS n FROM sessions WHERE "+emptySessionWhere)
	if err != nil {
		return 0, err
	}
	n, _ := rowValue(row, "n").(int64)
	return int(n), nil
}

// Deletes every empty ended unarchived session in one transaction, orphaning children and sweeping transcript files
func (d *SessionDB) DeleteEmptySessions(ctx context.Context, sessionsDir string) (int, error) {
	var removed []string
	count := 0
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		removed, count = nil, 0
		ids, err := queryStrings(ctx, conn, "SELECT id FROM sessions WHERE "+emptySessionWhere)
		if err != nil {
			return err
		}
		slices.Sort(ids)
		ids = slices.Compact(ids)
		quiet, err := retirePrunable(ctx, conn, ids)
		if err != nil || len(quiet) == 0 {
			return err
		}
		if err := deleteSessionRows(ctx, conn, quiet); err != nil {
			return err
		}
		removed = quiet
		if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
			return err
		}
		count = len(quiet)
		return nil
	}, nil)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		d.EmitSessionDeleted(ctx, removed, "empty_sweep", "", sessionsDir)
	}
	if err := d.CollectRetiredMedia(ctx); err != nil {
		return 0, err
	}
	for _, sid := range removed {
		removeSessionFiles(sessionsDir, sid)
	}
	return count, nil
}
