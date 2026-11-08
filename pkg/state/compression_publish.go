package state

import (
	"context"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var conversationModelConfigKeys = []string{"onboarding_run", "prompt_refresh_epoch"}

var unstampedEdgeChildSQL = `
    SELECT child.id FROM sessions parent JOIN sessions child ON child.parent_session_id = parent.id
    WHERE parent.end_reason = 'compression' AND ` + unsetCreatedSQL("parent.created_source") + `
      AND NOT ` + unsetCreatedSQL("child.created_source") + `
` + nonContinuationChildSQL("child.", "parent.id")

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
` + nonContinuationChildSQL("child.", "parent.id") + `
              ),
              descendants(id) AS (
                SELECT ?
                UNION
                SELECT child.id
                FROM descendants d
                JOIN sessions parent ON parent.id = d.id
                JOIN sessions child ON child.parent_session_id = parent.id
                WHERE parent.end_reason = 'compression'
` + nonContinuationChildSQL("child.", "parent.id") + `
              ),
              lineage(id) AS (
                SELECT id FROM ancestors
                UNION
                SELECT id FROM descendants
              )`

var (
	sessionSurfaces      = map[string]string{"cli": "cli", "tui": "tui", "gui": "desktop", "acp": "acp", "tool": "cli", "oneshot": "cli"}
	keepStoredLabel      = setOf("tool", "oneshot")
	createdNotASurface   = setOf("", "local", "unknown")
	nonContinuationChild = nonContinuationChildSQL("", "?")
)

// Created source of a policy or live label, nil for local, unknown and empty labels
func CreatedSurface(label any) any {
	s, ok := label.(string)
	if label == nil || (ok && createdNotASurface[s]) {
		return nil
	}
	if surface, known := sessionSurfaces[s]; ok && known && !keepStoredLabel[s] {
		return surface
	}
	return label
}

// Clears a stale explicit close stamp from a session a host proved still routed, returning the cleared reason
func (d *SessionDB) ReopenIfExplicitlyClosed(ctx context.Context, sessionID, provenance string, patienceS *float64) (*string, error) {
	if sessionID == "" {
		return nil, nil
	}
	var reason *string
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		reason = nil
		row, err := connRow(ctx, conn, endedRowSQL, sessionID)
		if err != nil || row == nil || row["ended_at"] == nil {
			return err
		}
		stamp := row["end_reason"]
		text, isText := stamp.(string)
		if IsAutomaticEndReason(stamp) || (isText && (text == "compression" || boundaryEndReasons[text])) {
			return nil
		}
		superseded, err := connRow(ctx, conn, "SELECT 1 AS one FROM sessions WHERE parent_session_id = ?"+nonContinuationChild+" LIMIT 1",
			sessionID, sessionID, sessionID, sessionID)
		if err != nil || superseded != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ? AND ended_at = ? AND end_reason = ?",
			sessionID, row["ended_at"], stamp); err != nil {
			return err
		}
		cleared := pyStr(stamp)
		reason = &cleared
		return nil
	}, patienceS)
	if err != nil {
		return nil, err
	}
	if reason != nil {
		slog.Warn("Session " + sessionID + " carried a stale " + pyRepr(*reason) + " end stamp while " + provenance +
			"; cleared so the conversation can compress and a later close is recorded (#106459)")
	}
	return reason, nil
}

// Stamps unset ancestors of chains compressed before write time stamping, returning the rows stamped
func (d *SessionDB) StampLegacyCompressionChains(ctx context.Context) (int64, error) {
	var stamped int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		stamped = 0
		rows, err := connRows(ctx, conn, unstampedEdgeChildSQL)
		if err != nil {
			return err
		}
		before, err := totalChanges(ctx, conn)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := stampCompressionAncestors(ctx, conn, pyStr(row["id"])); err != nil {
				return err
			}
		}
		after, err := totalChanges(ctx, conn)
		stamped = after - before
		return err
	}, nil)
	return stamped, err
}

// Total rows changed on the connection since it opened
func totalChanges(ctx context.Context, conn SQLiteConn) (int64, error) {
	var n int64
	err := conn.QueryRowContext(ctx, "SELECT total_changes()").Scan(&n)
	return n, err
}

// Unique live direct continuation of a compression ended parent, nil when none or several
func (d *SessionDB) FindLiveCompressionChild(ctx context.Context, parentSessionID string) (*pyjson.Object, error) {
	if parentSessionID == "" {
		return nil, nil
	}
	var rows []*pyjson.Object
	err := d.readCtx(ctx, true, func(conn SQLiteConn) error {
		rows = nil
		ended, err := connRow(ctx, conn, endedRowSQL, parentSessionID)
		if err != nil || !endedByCompression(ended) {
			return err
		}
		rows, err = queryObjects(ctx, conn, `
                SELECT s.*,
                       COALESCE(sp.prompt, s.system_prompt)
                           AS _system_prompt_resolved
                FROM sessions s
                LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash
                WHERE s.parent_session_id = ?
                  AND s.ended_at IS NULL
                `+nonContinuationChildSQL("s.", "?")+`
                ORDER BY s.started_at ASC
                LIMIT 2
                `, parentSessionID, parentSessionID, parentSessionID, parentSessionID)
		return err
	})
	if err != nil || len(rows) != 1 {
		return nil, err
	}
	return sessionRowDict(rows[0]), nil
}

// Reopens a compression parent only when no continuation was published and no live lease owns it
func (d *SessionDB) ReopenOrphanedCompressionSession(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	reopened := false
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		reopened, err = reopenOnConnection(ctx, conn, sessionID)
		return err
	}, nil)
	return reopened, err
}

// One durable compression child publication and its options, built with the hermes defaults by NewCompressionChildPublish
type CompressionChildPublish struct {
	ParentSessionID         string
	ChildSessionID          string
	Source                  string
	Messages                []map[string]any
	Model                   *string
	ModelConfig             *pyjson.Object
	SystemPrompt            *string
	Cwd                     *string
	ProfileName             *string
	CompressionLockHolder   *string
	RequireCompressionLease bool
	RequireLeaseRefresh     bool
	LeaseTTLSeconds         float64
	Watermark               *int64
	WatermarkCeiling        *int64
}

// Publication requiring the compression lease with a 300 second refresh ttl
func NewCompressionChildPublish(parentSessionID, childSessionID, source string, messages []map[string]any) CompressionChildPublish {
	return CompressionChildPublish{ParentSessionID: parentSessionID, ChildSessionID: childSessionID, Source: source, Messages: messages,
		RequireCompressionLease: true, LeaseTTLSeconds: 300}
}

// Optional string as a SQL value, nil for None
func optStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// First truthy value in order, the last one when none is
func pyOr(values ...any) any {
	for _, v := range values[:len(values)-1] {
		if pyTruthy(v) {
			return v
		}
	}
	return values[len(values)-1]
}

// Inserts the compression child's sessions row copied from the parent, conversation keys carried for a continuation
func (d *SessionDB) publishChildSessionRow(ctx context.Context, conn SQLiteConn, parent map[string]any, p CompressionChildPublish) error {
	continuation := true
	if p.ModelConfig != nil {
		for _, k := range []string{"_branched_from", "_reset_from"} {
			if _, ok := p.ModelConfig.Get(k); ok {
				continuation = false
			}
		}
	}
	modelConfig := pyjson.NewObject()
	parentConfig := parseModelConfigValue(parent["model_config"])
	for _, k := range parentConfig.Keys {
		if continuation && strings.Contains(" "+strings.Join(conversationModelConfigKeys, " ")+" ", " "+k+" ") {
			modelConfig.Set(k, parentConfig.Vals[k])
		}
	}
	if p.ModelConfig != nil {
		for _, k := range p.ModelConfig.Keys {
			modelConfig.Set(k, p.ModelConfig.Vals[k])
		}
	}
	hash, err := storeSystemPrompt(ctx, conn, optStr(p.SystemPrompt))
	if err != nil {
		return err
	}
	createdSource := CreatedSurface(parent["created_source"])
	if !pyTruthy(createdSource) {
		createdSource = CreatedSurface(p.Source)
	}
	var configText any
	if modelConfig.Len() > 0 {
		text, err := pyjson.Dumps(modelConfig)
		if err != nil {
			return err
		}
		configText = text
	}
	var ownProfile any
	if name := d.ownProfileName(); name != "" {
		ownProfile = name
	}
	pinned, archived, autoArchived := any(int64(0)), any(int64(0)), any(int64(0))
	if continuation {
		n, err := pyint.Of(pyOr(parent["pinned"], int64(0)))
		if err != nil {
			return err
		}
		pinned = n.Int64()
		archived = pyOr(parent["archived"], int64(0))
		autoArchived = pyOr(parent["auto_archived"], int64(0))
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO sessions (
                   id, source, created_source, model, model_config, system_prompt,
                   system_prompt_hash, tool_names,
                   parent_session_id, cwd, git_branch, git_repo_root,
                   profile_name, user_id, session_key, chat_id, chat_type,
                   thread_id, display_name, origin_json, pinned, started_at,
                   archived, auto_archived
                ) VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ChildSessionID, p.Source, createdSource, optStr(p.Model), configText, hash, parent["tool_names"], p.ParentSessionID,
		pyOr(optStr(p.Cwd), parent["cwd"]), parent["git_branch"], parent["git_repo_root"],
		pyOr(optStr(p.ProfileName), parent["profile_name"], ownProfile), parent["user_id"], parent["session_key"], parent["chat_id"],
		parent["chat_type"], parent["thread_id"], parent["display_name"], parent["origin_json"], pinned, now(), archived, autoArchived)
	return err
}

// Atomically closes a parent and publishes its durable compression child with the handoff and concurrent tail
func (d *SessionDB) PublishCompressionChild(ctx context.Context, p CompressionChildPublish) error {
	return d.executeTranscriptWrite(ctx, func(conn SQLiteConn) error {
		return d.publishCompressionChildOnConn(ctx, conn, p)
	}, p.Messages, nil)
}

// Publication body on the caller's write transaction, shared by the owner and the worker verbs
func (d *SessionDB) publishCompressionChildOnConn(ctx context.Context, conn SQLiteConn, p CompressionChildPublish) error {
	holder := ""
	if p.CompressionLockHolder != nil {
		holder = *p.CompressionLockHolder
	}
	if p.RequireLeaseRefresh && holder != "" {
		if _, err := conn.ExecContext(ctx, "UPDATE compression_locks SET expires_at = ? WHERE session_id = ? AND holder = ?",
			now()+p.LeaseTTLSeconds, p.ParentSessionID, holder); err != nil {
			return err
		}
	}
	lock, err := connRow(ctx, conn, compressionLockRowSQL, p.ParentSessionID)
	if err != nil {
		return err
	}
	if p.RequireCompressionLease {
		lost := lock == nil || holder == "" || !pyEquals(lock["holder"], holder)
		if !lost {
			exp, err := pyint.Float(lock["expires_at"])
			if err != nil {
				return err
			}
			lost = exp <= now()
		}
		if lost {
			return &CompressionSessionBusyError{Msg: "Compression lease lost before publication: " + p.ParentSessionID}
		}
	}
	parent, err := connRow(ctx, conn, `SELECT ended_at, end_reason, cwd, git_branch, git_repo_root,
                      user_id, session_key, chat_id, chat_type,
                      thread_id, display_name, origin_json, profile_name, tool_names,
                      archived, auto_archived, pinned, model_config, created_source
               FROM sessions WHERE id = ?`, p.ParentSessionID)
	if err != nil {
		return err
	}
	if parent == nil {
		return &pyint.Error{Kind: "RuntimeError", Msg: "Compression parent not found: " + p.ParentSessionID}
	}
	if parent["ended_at"] != nil {
		if !IsAutomaticEndReason(parent["end_reason"]) {
			return &pyint.Error{Kind: "RuntimeError", Msg: "Compression parent already ended: " + p.ParentSessionID}
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ?", p.ParentSessionID); err != nil {
			return err
		}
	}
	if len(p.Messages) == 0 {
		return &pyint.Error{Kind: "RuntimeError", Msg: "Compression child handoff must not be empty"}
	}
	if err := d.publishChildSessionRow(ctx, conn, parent, p); err != nil {
		return err
	}
	d.carryParentTimestamps(ctx, conn, p.ParentSessionID, p.Messages)
	totalMessages, totalToolCalls, err := d.insertMessageRows(ctx, conn, p.ChildSessionID, p.Messages, true)
	if err != nil {
		return err
	}
	if p.Watermark != nil {
		query := "SELECT id, tool_calls FROM messages WHERE session_id = ? AND active = 1 AND id > ?"
		args := []any{p.ParentSessionID, *p.Watermark}
		if p.WatermarkCeiling != nil {
			query += " AND id <= ?"
			args = append(args, *p.WatermarkCeiling)
		}
		tailIDs, tailToolCalls, err := tailRowsAfterWatermark(ctx, conn, query+" ORDER BY id", args...)
		if err != nil {
			return err
		}
		if len(tailIDs) > 0 {
			child := p.ChildSessionID
			if err := d.cloneMessageRows(ctx, conn, tailIDs, &child); err != nil {
				return err
			}
			totalMessages += len(tailIDs)
			totalToolCalls += tailToolCalls
		}
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET message_count = ?, tool_call_count = ? WHERE id = ?",
		totalMessages, totalToolCalls, p.ChildSessionID); err != nil {
		return err
	}
	res, err := conn.ExecContext(ctx, "UPDATE sessions SET ended_at = ?, end_reason = 'compression' WHERE id = ? AND ended_at IS NULL",
		now(), p.ParentSessionID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return &pyint.Error{Kind: "RuntimeError", Msg: "Compression parent changed during publication: " + p.ParentSessionID}
	}
	if err := stampCompressionAncestors(ctx, conn, p.ChildSessionID); err != nil {
		return err
	}
	if err := advanceLocalTarget(ctx, conn, p.ParentSessionID, p.ChildSessionID, nil); err != nil {
		return err
	}
	if pyTruthy(parent["archived"]) {
		if _, err := unarchiveAutoArchivedLineage(ctx, conn, p.ChildSessionID); err != nil {
			return err
		}
	}
	return nil
}

// Un-hides a lineage the idle sweep archived unless any row of it was archived deliberately
func unarchiveAutoArchivedLineage(ctx context.Context, conn SQLiteConn, sessionID string) (bool, error) {
	manual, err := connRow(ctx, conn, lineageCTESQL+`
            SELECT 1 AS one FROM sessions
            WHERE id IN (SELECT id FROM lineage) AND archived <> 0 AND COALESCE(auto_archived, 0) = 0
            LIMIT 1
            `, sessionID, sessionID)
	if err != nil || manual != nil {
		return false, err
	}
	res, err := conn.ExecContext(ctx, lineageCTESQL+`
            UPDATE sessions SET archived = 0, auto_archived = 0
            WHERE id IN (SELECT id FROM lineage) AND archived <> 0
            `, sessionID, sessionID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Marks week old message bearing continuations of compression parents that never ran an API call as orphaned
func (d *SessionDB) FinalizeOrphanedCompressionSessions(ctx context.Context) (int64, error) {
	cutoff := now() - 604800
	return d.writeRowcount(ctx, `
                UPDATE sessions
                SET ended_at = ?,
                    end_reason = 'orphaned_compression'
                WHERE api_call_count = 0
                  AND end_reason IS NULL
                  AND ended_at IS NULL
                  AND started_at < ?
                  AND parent_session_id IS NOT NULL
                  AND EXISTS (
                      SELECT 1 FROM sessions p
                      WHERE p.id = sessions.parent_session_id
                        AND p.end_reason = 'compression'
                        AND p.ended_at IS NOT NULL
                  )
                  AND EXISTS (
                      SELECT 1 FROM messages m
                      WHERE m.session_id = sessions.id
                  )
                `, []any{now(), cutoff}, nil)
}

// Compression ancestors through the tip in chronological order, later tips only when the session itself was compacted
func (d *SessionDB) GetCompressionLineage(ctx context.Context, sessionID string) ([]string, error) {
	session, err := d.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return []string{}, nil
	}
	if isExplicitForkChildRow(session, false) {
		return []string{sessionID}, nil
	}
	root := session
	ancestors := map[string]bool{rowStrOr(root, "id"): true}
	for {
		isChild, err := d.isCompressionChildRow(ctx, root)
		if err != nil {
			return nil, err
		}
		if !isChild {
			break
		}
		parent, err := d.GetSession(ctx, pyStr(rowValue(root, "parent_session_id")))
		if err != nil {
			return nil, err
		}
		if parent == nil || ancestors[rowStrOr(parent, "id")] {
			break
		}
		root = parent
		ancestors[rowStrOr(root, "id")] = true
	}
	lineage := []string{pyStr(rowValue(root, "id"))}
	seen := map[string]bool{lineage[0]: true}
	current := root
	for pyEquals(rowValue(current, "end_reason"), "compression") {
		rows, err := d.readAll(ctx, "\n                SELECT * FROM sessions\n                WHERE parent_session_id = ?\n                ORDER BY started_at ASC\n                ",
			rowValue(current, "id"))
		if err != nil {
			return nil, err
		}
		var next *pyjson.Object
		for _, row := range rows {
			ok, err := d.isCompressionChildRow(ctx, row)
			if err != nil {
				return nil, err
			}
			if ok {
				next = row
				break
			}
		}
		if next == nil || seen[pyStr(rowValue(next, "id"))] {
			break
		}
		id := pyStr(rowValue(next, "id"))
		lineage = append(lineage, id)
		seen[id] = true
		current = next
	}
	for _, id := range lineage {
		if id == sessionID {
			return lineage, nil
		}
	}
	return []string{sessionID}, nil
}
