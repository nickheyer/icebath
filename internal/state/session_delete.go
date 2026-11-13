package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Delete an active turn lease or compression lock refused
type ActiveWriteGuardError struct{ SessionID string }

// Session that stayed
func (e *ActiveWriteGuardError) Error() string {
	return fmt.Sprintf("session '%s' (or a delegate child) has an active turn lease or compression lock", e.SessionID)
}

// How a single session delete is fenced and what it sweeps
type DeleteOptions struct {
	// Transcript directory whose files of every removed id are swept, empty sweeping none
	SessionsDir string
	// Exact ids the delete must remove, nil when no drift fence applies
	ExpectedDeleteIDs []string
	// Runs inside the delete transaction, false aborting the delete because the transcript drifted
	TranscriptFence func(ctx context.Context, c Conn) (bool, error)
	// Refuses with ActiveWriteGuardError when a lease or lock protects any row the delete would remove
	ExcludeActiveWriteGuards bool
	// Deletes the whole compression chain the session belongs to
	IncludeCompressionChain bool
}

// Rows a delete would remove, the session, the rest of its local lineage, then its delegate children
func (s *Store) GetSessionDeleteTargets(ctx context.Context, sessionID string) ([]string, error) {
	var out []string
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ? LIMIT 1", sessionID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		scope, err := ownedLineageIDs(ctx, c, sessionID)
		if err != nil {
			return err
		}
		delegates, err := collectDelegateChildIDs(ctx, c, scope)
		if err != nil {
			return err
		}
		var rest []string
		for _, id := range scope {
			if id != sessionID {
				rest = append(rest, id)
			}
		}
		sort.Strings(rest)
		sort.Strings(delegates)
		out = append(append([]string{sessionID}, rest...), delegates...)
		return nil
	})
	return out, err
}

// Deletes a session and its messages, delegate children cascading and branch or compression children orphaned
func (s *Store) DeleteSession(ctx context.Context, sessionID string, o DeleteOptions) (bool, error) {
	if o.IncludeCompressionChain {
		n, _, err := s.DeleteSessions(ctx, []string{sessionID}, DeleteManyOptions{
			SessionsDir: o.SessionsDir, ExcludeActiveWriteGuards: o.ExcludeActiveWriteGuards, IncludeCompressionChain: true,
		})
		return n > 0, err
	}
	var removed []string
	deleted := false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		removed, deleted = nil, false
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ? LIMIT 1", sessionID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		scope, err := ownedLineageIDs(ctx, c, sessionID)
		if err != nil {
			return err
		}
		if o.ExcludeActiveWriteGuards || o.ExpectedDeleteIDs != nil {
			delegates, err := collectDelegateChildIDs(ctx, c, scope)
			if err != nil {
				return err
			}
			targets := append(append([]string{}, scope...), delegates...)
			if o.ExcludeActiveWriteGuards {
				guarded, err := s.guardedIDs(ctx, c, targets)
				if err != nil {
					return err
				}
				if len(guarded) > 0 {
					return &ActiveWriteGuardError{SessionID: sessionID}
				}
			}
			if o.ExpectedDeleteIDs != nil && !sameIDSet(o.ExpectedDeleteIDs, targets) {
				return nil
			}
		}
		if o.TranscriptFence != nil {
			ok, err := o.TranscriptFence(ctx, c)
			if err != nil || !ok {
				return err
			}
		}
		if err := retireSessions(ctx, c, scope, nil); err != nil {
			return err
		}
		delegates, err := deleteDelegateChildren(ctx, c, scope)
		if err != nil {
			return err
		}
		removed = append(removed, delegates...)
		for _, sid := range scope {
			if _, err := c.ExecContext(ctx, "UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id = ?", sid); err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sid); err != nil {
				return err
			}
		}
		for _, sid := range scope {
			if _, err := c.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", sid); err != nil {
				return err
			}
		}
		if err := deleteUnreferencedSystemPrompts(ctx, c); err != nil {
			return err
		}
		removed = append(removed, scope...)
		deleted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, s.afterDelete(ctx, deleted, removed, DeleteReasonDelete, sessionID, o.SessionsDir)
}

// Emits the committed delete, collects retired media and sweeps transcript files of every removed id
func (s *Store) afterDelete(ctx context.Context, deleted bool, removed []string, reason, primary, sessionsDir string) error {
	if deleted {
		s.emitSessionDeleted(ctx, removed, reason, primary, sessionsDir)
	}
	if err := s.collectRetiredMedia(ctx); err != nil {
		return err
	}
	for _, sid := range removed {
		removeSessionFiles(sessionsDir, sid)
	}
	return nil
}

// Reports whether two id lists hold the same set
func sameIDSet(a, b []string) bool {
	set := map[string]bool{}
	for _, v := range a {
		set[v] = true
	}
	other := map[string]bool{}
	for _, v := range b {
		other[v] = true
	}
	if len(set) != len(other) {
		return false
	}
	for v := range set {
		if !other[v] {
			return false
		}
	}
	return true
}

// Deletes the delegate subagent children of parents, retiring them first and orphaning untagged stragglers
func deleteDelegateChildren(ctx context.Context, c Conn, parentIDs []string) ([]string, error) {
	ids, err := collectDelegateChildIDs(ctx, c, parentIDs)
	if err != nil {
		return nil, err
	}
	if err := retireSessions(ctx, c, ids, nil); err != nil {
		return nil, err
	}
	for _, chunk := range chunks(ids, sqlInChunk) {
		ph, args := bindMarks(len(chunk)), stringArgs(chunk)
		for _, q := range []string{
			"DELETE FROM messages WHERE session_id IN (" + ph + ")",
			"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IN (" + ph + ")",
			"DELETE FROM sessions WHERE id IN (" + ph + ")",
		} {
			if _, err := c.ExecContext(ctx, q, args...); err != nil {
				return nil, err
			}
		}
	}
	return ids, nil
}

// Removes a session's transcript snapshots and request dumps, a filesystem failure never blocking the delete
func removeSessionFiles(sessionsDir, sessionID string) {
	if sessionsDir == "" {
		return
	}
	targets := []string{
		filepath.Join(sessionsDir, sessionID+".json"), filepath.Join(sessionsDir, sessionID+".jsonl"),
		filepath.Join(sessionsDir, "session_"+sessionID+".json"),
	}
	if entries, err := os.ReadDir(sessionsDir); err == nil {
		prefix := "request_dump_" + sessionID + "_"
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".json") && len(name) >= len(prefix)+len(".json") {
				targets = append(targets, filepath.Join(sessionsDir, name))
			}
		}
	}
	for _, p := range targets {
		_ = os.Remove(p)
	}
}

// Aborts a row this flow just created that nothing was admitted against, falling back to the fenced delete otherwise
func (s *Store) DiscardUnadmittedSession(ctx context.Context, sessionID string) (bool, error) {
	admitted, deleted := false, false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		admitted, deleted = false, false
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM session_admissions WHERE target_session_id=? UNION ALL "+
			"SELECT 1 FROM worker_executions WHERE session_id=? LIMIT 1", sessionID, sessionID).Scan(&one)
		if err == nil {
			admitted = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := c.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
			return err
		}
		res, err := c.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", sessionID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		deleted = n > 0
		return err
	})
	if err != nil {
		return false, err
	}
	if admitted {
		return s.DeleteSession(ctx, sessionID, DeleteOptions{})
	}
	return deleted, nil
}

// Deletes a session only when it has no messages, title or children and no lease or lock guards it
func (s *Store) DeleteSessionIfEmpty(ctx context.Context, sessionID, sessionsDir string) (bool, error) {
	deleted := false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		deleted = false
		guarded, err := s.guardedIDs(ctx, c, []string{sessionID})
		if err != nil || len(guarded) > 0 {
			return err
		}
		scope, err := ownedLineageIDs(ctx, c, sessionID)
		if err != nil || len(scope) != 1 || scope[0] != sessionID {
			return err
		}
		var one int
		err = c.QueryRowContext(ctx, `
                SELECT 1 FROM sessions
                WHERE id = ?
                  AND title IS NULL
                  AND NOT EXISTS (
                      SELECT 1 FROM messages WHERE messages.session_id = sessions.id
                  )
                  AND NOT EXISTS (
                      SELECT 1 FROM sessions child
                      WHERE child.parent_session_id = sessions.id
                  )
                `, sessionID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := retireSessions(ctx, c, []string{sessionID}, []string{sessionID}); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", sessionID); err != nil {
			return err
		}
		if err := deleteUnreferencedSystemPrompts(ctx, c); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if deleted {
		s.emitSessionDeleted(ctx, []string{sessionID}, DeleteReasonEmptySweep, "", sessionsDir)
	}
	if err := s.collectRetiredMedia(ctx); err != nil {
		return deleted, err
	}
	if deleted {
		removeSessionFiles(sessionsDir, sessionID)
	}
	return deleted, nil
}

// How a bulk delete is fenced and what it sweeps
type DeleteManyOptions struct {
	SessionsDir string
	// Skips rows a lease or lock protects instead of refusing, reporting them back
	ExcludeActiveWriteGuards bool
	// Expands each id to its whole compression chain
	IncludeCompressionChain bool
}

// Bulk delete with single delete semantics in one transaction, returning the requested rows deleted and the guarded ones skipped
func (s *Store) DeleteSessions(ctx context.Context, sessionIDs []string, o DeleteManyOptions) (int, []string, error) {
	var unique []string
	for _, id := range sessionIDs {
		if id != "" && !containsString(unique, id) {
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 {
		return 0, nil, nil
	}
	var removed, skipped []string
	count := 0
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		removed, skipped, count = nil, nil, 0
		var existing []string
		for _, chunk := range chunks(unique, sqlInChunk) {
			ids, err := queryStrings(ctx, c, "SELECT id FROM sessions WHERE id IN ("+bindMarks(len(chunk))+")", stringArgs(chunk)...)
			if err != nil {
				return err
			}
			existing = append(existing, ids...)
		}
		if len(existing) == 0 {
			return nil
		}
		expand := func(ids []string) ([]string, error) {
			if o.IncludeCompressionChain {
				return s.expandCompressionLineage(ctx, c, ids)
			}
			var out []string
			for _, id := range ids {
				owned, err := ownedLineageIDs(ctx, c, id)
				if err != nil {
					return nil, err
				}
				for _, x := range owned {
					if !containsString(out, x) {
						out = append(out, x)
					}
				}
			}
			return out, nil
		}
		expanded, err := expand(existing)
		if err != nil {
			return err
		}
		if o.ExcludeActiveWriteGuards {
			if existing, skipped, err = s.dropGuardedRoots(ctx, c, existing, expanded, expand); err != nil || len(existing) == 0 {
				return err
			}
			if expanded, err = expand(existing); err != nil {
				return err
			}
		}
		if err := retireSessions(ctx, c, expanded, nil); err != nil {
			return err
		}
		delegates, err := deleteDelegateChildren(ctx, c, expanded)
		if err != nil {
			return err
		}
		removed = append(removed, delegates...)
		for _, chunk := range chunks(expanded, sqlInChunk) {
			ph, args := bindMarks(len(chunk)), stringArgs(chunk)
			for _, q := range []string{
				"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IN (" + ph + ")",
				"DELETE FROM messages WHERE session_id IN (" + ph + ")",
				"DELETE FROM sessions WHERE id IN (" + ph + ")",
			} {
				if _, err := c.ExecContext(ctx, q, args...); err != nil {
					return err
				}
			}
		}
		if err := deleteUnreferencedSystemPrompts(ctx, c); err != nil {
			return err
		}
		removed = append(removed, expanded...)
		count = len(existing)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	primary := ""
	if len(unique) == 1 {
		primary = unique[0]
	}
	return count, skipped, s.afterDelete(ctx, count > 0, removed, DeleteReasonDelete, primary, o.SessionsDir)
}

// Selected roots with every guarded lineage member removed, and the sorted roots it skipped
func (s *Store) dropGuardedRoots(ctx context.Context, c Conn, existing, expanded []string, expand func([]string) ([]string, error)) ([]string, []string, error) {
	delegates, err := collectDelegateChildIDs(ctx, c, expanded)
	if err != nil {
		return nil, nil, err
	}
	guarded, err := s.guardedIDs(ctx, c, append(append([]string{}, expanded...), delegates...))
	if err != nil || len(guarded) == 0 {
		return existing, nil, err
	}
	var kept, skipped []string
	for _, sid := range existing {
		members, err := expand([]string{sid})
		if err != nil {
			return nil, nil, err
		}
		if !containsString(members, sid) {
			members = append(members, sid)
		}
		children, err := collectDelegateChildIDs(ctx, c, members)
		if err != nil {
			return nil, nil, err
		}
		hit, err := s.guardedIDs(ctx, c, append(members, children...))
		if err != nil {
			return nil, nil, err
		}
		if len(hit) > 0 {
			skipped = append(skipped, sid)
		} else {
			kept = append(kept, sid)
		}
	}
	sort.Strings(skipped)
	return kept, skipped, nil
}

// Every member of the compression chains of ids in both directions, local reset lineages included
func (s *Store) expandCompressionLineage(ctx context.Context, c Conn, ids []string) ([]string, error) {
	seen := append([]string{}, ids...)
	frontier := append([]string{}, ids...)
	for range chainCap {
		if len(frontier) == 0 {
			break
		}
		targets, err := expandCompressionLineageIDs(ctx, c, frontier)
		if err != nil {
			return nil, err
		}
		frontier = nil
		for _, id := range targets {
			if !containsString(seen, id) && !containsString(frontier, id) {
				frontier = append(frontier, id)
			}
		}
		seen = append(seen, frontier...)
	}
	for _, sid := range append([]string{}, seen...) {
		conv, err := localConversationIDs(ctx, c, sid)
		if err != nil {
			return nil, err
		}
		for _, id := range conv {
			if !containsString(seen, id) {
				seen = append(seen, id)
			}
		}
	}
	return seen, nil
}

// Count of empty ended non-archived sessions
func (s *Store) CountEmptySessions(ctx context.Context) (int64, error) {
	var n int64
	err := s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE "+emptySessionWhere).Scan(&n)
	})
	return n, err
}

// Deletes every empty ended non-archived session in one transaction, orphaning their children and sweeping files
func (s *Store) DeleteEmptySessions(ctx context.Context, sessionsDir string) (int, error) {
	var removed []string
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		removed = nil
		ids, err := queryStrings(ctx, c, "SELECT id FROM sessions WHERE "+emptySessionWhere)
		if err != nil {
			return err
		}
		sort.Strings(ids)
		if ids, err = retirePrunable(ctx, c, ids); err != nil || len(ids) == 0 {
			return err
		}
		for _, chunk := range chunks(ids, sqlInChunk) {
			ph, args := bindMarks(len(chunk)), stringArgs(chunk)
			for _, q := range []string{
				"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IN (" + ph + ")",
				"DELETE FROM messages WHERE session_id IN (" + ph + ")",
				"DELETE FROM sessions WHERE id IN (" + ph + ")",
			} {
				if _, err := c.ExecContext(ctx, q, args...); err != nil {
					return err
				}
			}
			removed = append(removed, chunk...)
		}
		return deleteUnreferencedSystemPrompts(ctx, c)
	})
	if err != nil {
		return 0, err
	}
	return len(removed), s.afterDelete(ctx, len(removed) > 0, removed, DeleteReasonEmptySweep, "", sessionsDir)
}

// Archives every lineage the prune selection matches, each judged through its tip, returning how many matched
func (s *Store) ArchiveSessions(ctx context.Context, sel ArchiveSelection) (int, error) {
	ids, err := s.archiveCandidates(ctx, sel)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := s.SetSessionArchived(ctx, id, true, true); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
