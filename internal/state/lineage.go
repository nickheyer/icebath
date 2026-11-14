package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Defensive bound on a forward compression chain walk, a seen set guarding cycles
const chainCap = 1000

// One forward step of a compression chain, the preferred continuation child of the bound parent
var chainStepSQL = `
                    SELECT child.id
                    FROM sessions parent
                    JOIN sessions child ON child.parent_session_id = parent.id
                    WHERE parent.id = ?
                      AND parent.end_reason = 'compression'
                      AND ` + JSONExtractSQL("child.model_config", "$._branched_from") + ` IS NULL
                      AND ` + JSONExtractSQL("child.model_config", "$._delegate_from") + ` IS NULL
                      AND NOT (` + ResetChildSQL("child") + `)
                      AND COALESCE(child.source, '') != 'tool'
                    ORDER BY
                      CASE
                        WHEN child.end_reason = 'compression' THEN 0
                        WHEN child.ended_at IS NULL THEN 1
                        ELSE 2
                      END,
                      ` + SessionLastActiveSQL("child") + ` DESC,
                      child.started_at DESC,
                      child.id DESC
                    LIMIT 1
                    `

// Compression lineage of the session bound twice, ancestors and continuations over compression edges only
var lineageCTESQL = `
            WITH RECURSIVE
              ancestors(id) AS (
                SELECT ?
                UNION
                SELECT parent.id
                FROM ancestors a
                JOIN sessions child ON child.id = a.id
                JOIN sessions parent ON parent.id = child.parent_session_id
                WHERE parent.end_reason = 'compression'
` + nonContinuationChildSQL("child.", "parent.id") + `              ),
              descendants(id) AS (
                SELECT ?
                UNION
                SELECT child.id
                FROM descendants d
                JOIN sessions parent ON parent.id = d.id
                JOIN sessions child ON child.parent_session_id = parent.id
                WHERE parent.end_reason = 'compression'
` + nonContinuationChildSQL("child.", "parent.id") + `              ),
              lineage(id) AS (
                SELECT id FROM ancestors
                UNION
                SELECT id FROM descendants
              )`

// Root first ids from sessionID through its tip, step returning the preferred continuation or empty
func walkCompressionChain(sessionID string, step func(id string) (string, error)) ([]string, error) {
	if sessionID == "" {
		return nil, nil
	}
	chain := []string{sessionID}
	seen := map[string]bool{sessionID: true}
	current := sessionID
	for range chainCap {
		child, err := step(current)
		if err != nil {
			return chain, err
		}
		if child == "" || seen[child] {
			return chain, nil
		}
		seen[child] = true
		current = child
		chain = append(chain, child)
	}
	return chain, nil
}

// Preferred continuation child of parentID on c, empty when it has none
func chainStep(ctx context.Context, c Conn, parentID string) (string, error) {
	var id sql.NullString
	err := c.QueryRowContext(ctx, chainStepSQL, parentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id.String, err
}

// Compression continuation chain root first through its tip, the session alone when nothing continues it
func (s *Store) CompressionChain(ctx context.Context, sessionID string) ([]string, error) {
	return walkCompressionChain(sessionID, func(id string) (string, error) {
		var child string
		err := s.Read(ctx, func(c Conn) error {
			var err error
			child, err = chainStep(ctx, c, id)
			return err
		})
		return child, err
	})
}

// Live tip of the compression chain of sessionID, the input id when no continuation exists
func (s *Store) CompressionTip(ctx context.Context, sessionID string) (string, error) {
	chain, err := s.CompressionChain(ctx, sessionID)
	if err != nil || len(chain) == 0 {
		return sessionID, err
	}
	return chain[len(chain)-1], nil
}

// Compression tip read on a writer's own connection so no rotation publishes in between
func CompressionTipIn(ctx context.Context, c Conn, sessionID string) (string, error) {
	chain, err := walkCompressionChain(sessionID, func(id string) (string, error) { return chainStep(ctx, c, id) })
	if err != nil {
		return "", err
	}
	if len(chain) == 0 {
		return sessionID, nil
	}
	return chain[len(chain)-1], nil
}

// Reports a branch, delegate or tool fork of its parent, optionally a reset fork, markers bound to the parent id
func isExplicitForkChildRow(s Session, includeReset bool) bool {
	if s.Source == "tool" {
		return true
	}
	t := strings.TrimSpace(s.ModelConfig.V)
	if !s.ModelConfig.Valid || t == "" || t[0] != '{' {
		return false
	}
	var cfg map[string]json.RawMessage
	if json.Unmarshal([]byte(t), &cfg) != nil {
		return false
	}
	keys := []string{"_branched_from", "_delegate_from"}
	if includeReset {
		keys = append(keys, "_reset_from")
	}
	for _, k := range keys {
		raw, ok := cfg[k]
		if !ok {
			continue
		}
		if !s.ParentSessionID.Valid || s.ParentSessionID.V == "" {
			if string(raw) != "null" {
				return true
			}
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) == nil && v == s.ParentSessionID.V {
			return true
		}
	}
	return false
}

// Reports whether the session is a branch, delegate or tool fork of its parent, a missing row being no fork
func (s *Store) IsExplicitForkChild(ctx context.Context, sessionID string) (bool, error) {
	row, ok, err := s.GetSession(ctx, sessionID)
	return ok && isExplicitForkChildRow(row, false), err
}

// Fork status and source of a session in one read, a missing row being no fork with no source
func (s *Store) DeclaredScopeIdentity(ctx context.Context, sessionID string) (bool, string, error) {
	row, ok, err := s.GetSession(ctx, sessionID)
	if err != nil || !ok {
		return false, "", err
	}
	return isExplicitForkChildRow(row, false), strings.TrimSpace(row.Source), nil
}

// Reports a genuine continuation, a non fork child whose parent ended by compression
func (s *Store) isCompressionChildRow(ctx context.Context, child Session) (bool, error) {
	if !child.ParentSessionID.Valid || child.ParentSessionID.V == "" || isExplicitForkChildRow(child, true) {
		return false, nil
	}
	parent, ok, err := s.GetSession(ctx, child.ParentSessionID.V)
	return ok && parent.EndReason.Valid && parent.EndReason.V == "compression", err
}

// Compression ancestors through the tip in order, later tips only when the requested session itself was compacted
func (s *Store) CompressionLineage(ctx context.Context, sessionID string) ([]string, error) {
	session, ok, err := s.GetSession(ctx, sessionID)
	if err != nil || !ok {
		return nil, err
	}
	if isExplicitForkChildRow(session, false) {
		return []string{sessionID}, nil
	}
	root := session
	ancestors := map[string]bool{root.ID: true}
	for {
		cont, err := s.isCompressionChildRow(ctx, root)
		if err != nil {
			return nil, err
		}
		if !cont {
			break
		}
		parent, ok, err := s.GetSession(ctx, root.ParentSessionID.V)
		if err != nil {
			return nil, err
		}
		if !ok || ancestors[parent.ID] {
			break
		}
		root = parent
		ancestors[root.ID] = true
	}
	lineage := []string{root.ID}
	seen := map[string]bool{root.ID: true}
	current := root
	for current.EndReason.Valid && current.EndReason.V == "compression" {
		var children []Session
		err := s.Read(ctx, func(c Conn) error {
			var err error
			children, err = querySessions(c.QueryContext(ctx, "\n                SELECT * FROM sessions\n                WHERE parent_session_id = ?\n                ORDER BY started_at ASC\n                ", current.ID))
			return err
		})
		if err != nil {
			return nil, err
		}
		var next *Session
		for i := range children {
			cont, err := s.isCompressionChildRow(ctx, children[i])
			if err != nil {
				return nil, err
			}
			if cont {
				next = &children[i]
				break
			}
		}
		if next == nil || seen[next.ID] {
			break
		}
		lineage = append(lineage, next.ID)
		seen[next.ID] = true
		current = *next
	}
	if !containsString(lineage, sessionID) {
		return []string{sessionID}, nil
	}
	return lineage, nil
}

// Session ids a display resume materializes, the verified compression lineage root to tip or the session alone
func (s *Store) ResumeLineageIDs(ctx context.Context, sessionID string) ([]string, error) {
	if sessionID == "" {
		return []string{sessionID}, nil
	}
	session, ok, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !ok || isExplicitForkChildRow(session, false) {
		return []string{sessionID}, nil
	}
	chain := []string{sessionID}
	seen := map[string]bool{sessionID: true}
	current := session
	for len(chain) < 100 {
		cont, err := s.isCompressionChildRow(ctx, current)
		if err != nil {
			return nil, err
		}
		if !cont {
			break
		}
		parent, ok, err := s.GetSession(ctx, current.ParentSessionID.V)
		if err != nil {
			return nil, err
		}
		if !ok || seen[parent.ID] {
			break
		}
		seen[parent.ID] = true
		chain = append(chain, parent.ID)
		current = parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// Raw parent walk root to tip, crossing every edge, bounded at a hundred hops
func (s *Store) sessionLineageRootToTip(ctx context.Context, sessionID string) ([]string, error) {
	if sessionID == "" {
		return []string{sessionID}, nil
	}
	var chain []string
	err := s.Read(ctx, func(c Conn) error {
		chain = chain[:0]
		current := sessionID
		for current != "" && !containsString(chain, current) && len(chain) < 100 {
			chain = append(chain, current)
			var parent sql.NullString
			err := c.QueryRowContext(ctx, "SELECT parent_session_id FROM sessions WHERE id = ?", current).Scan(&parent)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			current = parent.String
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return []string{sessionID}, nil
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// Reports a user facing branch that owns a copied transcript
func (s *Store) isExplicitBranchSession(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	var cfg sql.NullString
	found := false
	err := s.Read(ctx, func(c Conn) error {
		err := c.QueryRowContext(ctx, "SELECT model_config FROM sessions WHERE id = ?", sessionID).Scan(&cfg)
		found = err == nil
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil || !found {
		return false, err
	}
	return jsonTruthy(ParseModelConfig(cfg.String)["_branched_from"]), nil
}

// Created source, parent and an unset ancestor probe of the creator stamp walk
const (
	createdRowSQL      = "SELECT parent_session_id, created_source FROM sessions WHERE id = ?"
	unsetAncestorSQLHd = `
    WITH RECURSIVE up(pid, n) AS (
        SELECT parent_session_id, 1 FROM sessions WHERE id = ?
        UNION ALL
        SELECT s.parent_session_id, up.n + 1 FROM up JOIN sessions s ON s.id = up.pid
        WHERE s.end_reason = 'compression' AND s.created_source = ? AND up.n < ?
    )
    SELECT 1 FROM up CROSS JOIN sessions s ON s.id = up.pid
    WHERE s.end_reason = 'compression' AND `
)

// Reports a created_source value that carries no creation provenance
func unsetCreated(v sql.NullString) bool {
	return !v.Valid || v.String == "" || v.String == "unknown" || v.String == "local"
}

// Copies the session's created_source onto compression ancestors that have none, each hop the parent's preferred continuation
func stampCompressionAncestors(ctx context.Context, c Conn, sessionID string) error {
	var parent, surface sql.NullString
	err := c.QueryRowContext(ctx, createdRowSQL, sessionID).Scan(&parent, &surface)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || unsetCreated(surface) {
		return err
	}
	var one int
	err = c.QueryRowContext(ctx, unsetAncestorSQLHd+unsetCreatedSQL("s.created_source")+"\n    LIMIT 1\n", sessionID, surface.String, chainCap).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	current, parentID := sessionID, parent
	for range chainCap {
		if !parentID.Valid || parentID.String == "" {
			return nil
		}
		var grand, created sql.NullString
		err := c.QueryRowContext(ctx, createdRowSQL, parentID.String).Scan(&grand, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !unsetCreated(created) && created.String != surface.String {
			return nil
		}
		step, err := chainStep(ctx, c, parentID.String)
		if err != nil || step != current {
			return err
		}
		if created.String != surface.String || !created.Valid {
			if _, err := c.ExecContext(ctx, "UPDATE sessions SET created_source = ? WHERE id = ?", surface.String, parentID.String); err != nil {
				return err
			}
		}
		current, parentID = parentID.String, grand
	}
	return nil
}

// Delegate subagent ids to cascade with parentIDs, walked through their markers, never the parents themselves
func collectDelegateChildIDs(ctx context.Context, c Conn, parentIDs []string) ([]string, error) {
	df := delegateFromSQL("model_config")
	seeds := map[string]bool{}
	var frontier []string
	for _, id := range parentIDs {
		if id != "" && !seeds[id] {
			seeds[id] = true
			frontier = append(frontier, id)
		}
	}
	found := map[string]bool{}
	for id := range seeds {
		found[id] = true
	}
	var children []string
	for len(frontier) > 0 {
		var next []string
		for _, chunk := range chunks(frontier, sqlInChunk/2) {
			ph := bindMarks(len(chunk))
			ids, err := queryStrings(ctx, c, "SELECT id FROM sessions WHERE "+df+" IN ("+ph+") OR (parent_session_id IN ("+ph+") AND "+df+" IS NOT NULL)",
				append(stringArgs(chunk), stringArgs(chunk)...)...)
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				if !found[id] {
					found[id] = true
					next = append(next, id)
					children = append(children, id)
				}
			}
		}
		frontier = next
	}
	return children, nil
}

// One both direction compression edge step from frontier, branch, reset, delegate and tool children excluded
func expandCompressionLineageIDs(ctx context.Context, c Conn, frontier []string) ([]string, error) {
	var ids []string
	for _, id := range frontier {
		if id != "" {
			ids = append(ids, id)
		}
	}
	childFilter := JSONExtractSQL("child.model_config", "$._branched_from") + " IS NULL " +
		"AND " + JSONExtractSQL("child.model_config", "$._delegate_from") + " IS NULL " +
		"AND NOT (" + ResetChildSQL("child") + ") " +
		"AND COALESCE(child.source, '') != 'tool'"
	var out []string
	for _, chunk := range chunks(ids, sqlInChunk/4) {
		ph := bindMarks(len(chunk))
		forward, err := queryStrings(ctx, c, "SELECT child.id FROM sessions parent "+
			"JOIN sessions child ON child.parent_session_id = parent.id "+
			"WHERE parent.id IN ("+ph+") AND parent.end_reason = 'compression' AND "+childFilter, stringArgs(chunk)...)
		if err != nil {
			return nil, err
		}
		back, err := queryStrings(ctx, c, "SELECT parent.id FROM sessions child "+
			"JOIN sessions parent ON parent.id = child.parent_session_id "+
			"WHERE child.id IN ("+ph+") AND parent.end_reason = 'compression' "+
			"AND "+childFilter, stringArgs(chunk)...)
		if err != nil {
			return nil, err
		}
		out = append(append(out, forward...), back...)
	}
	return out, nil
}
