package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	cooldownRowSQL           = "SELECT compression_failure_cooldown_until, compression_failure_error FROM sessions WHERE id = ?"
	turnLeaseSweepGrace      = 86400.0
	turnLeasePollInterval    = time.Second
	turnLeaseNoticeInterval  = 15 * time.Second
	turnLeaseAcquirePatience = 500 * time.Millisecond
	orphanedCompressionAge   = 604800.0

	DefaultCompressionLockTTL = 300 * time.Second
)

var conversationModelConfigKeys = []string{"onboarding_run", "prompt_refresh_epoch"}

var unstampedEdgeChildSQL = `
    SELECT child.id FROM sessions parent JOIN sessions child ON child.parent_session_id = parent.id
    WHERE parent.end_reason = 'compression' AND ` + unsetCreatedSQL("parent.created_source") + `
      AND NOT ` + unsetCreatedSQL("child.created_source") + "\n" + nonContinuationChildSQL("child.", "parent.id")

var nonContinuationChildFilter = nonContinuationChildSQL("", "?")

type CompressionPublishError struct{ msg string }

func (e *CompressionPublishError) Error() string { return e.msg }

func (s *Store) ReopenIfExplicitlyClosed(ctx context.Context, sessionID, provenance string, patience time.Duration) (string, bool, error) {
	if sessionID == "" {
		return "", false, nil
	}
	if patience <= 0 {
		patience = WritePatience
	}
	var reason sql.NullString
	cleared := false
	err := s.Write(ctx, patience, func(c Conn) error {
		cleared = false
		var endedAt any
		err := c.QueryRowContext(ctx, endedRowSQL, sessionID).Scan(&endedAt, &reason)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil || endedAt == nil {
			return err
		}
		if IsAutomaticEndReason(reason.String) || reason.String == "compression" || IsBoundaryEndReason(reason.String) {
			return nil
		}
		var one int
		err = c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE parent_session_id = ?"+nonContinuationChildFilter+" LIMIT 1",
			sessionID, sessionID, sessionID, sessionID).Scan(&one)
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ? AND ended_at = ? AND end_reason = ?",
			sessionID, endedAt, reason); err != nil {
			return err
		}
		cleared = true
		return nil
	})
	if err != nil || !cleared {
		return "", false, err
	}
	slog.Warn(fmt.Sprintf("Session %s carried a stale %s end stamp while %s; cleared so the conversation can "+
		"compress and a later close is recorded (#106459)", sessionID, quotedText(reason.String), provenance))
	return reason.String, true, nil
}

func (s *Store) StampLegacyCompressionChains(ctx context.Context) (int64, error) {
	var stamped int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var before, after int64
		if err := c.QueryRowContext(ctx, "SELECT total_changes()").Scan(&before); err != nil {
			return err
		}
		ids, err := queryStrings(ctx, c, unstampedEdgeChildSQL)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := stampCompressionAncestors(ctx, c, id); err != nil {
				return err
			}
		}
		if err := c.QueryRowContext(ctx, "SELECT total_changes()").Scan(&after); err != nil {
			return err
		}
		stamped = after - before
		return nil
	})
	return stamped, err
}

func (s *Store) FindLiveCompressionChild(ctx context.Context, parentSessionID string) (Session, bool, error) {
	if parentSessionID == "" {
		return Session{}, false, nil
	}
	var found []Session
	err := s.Read(ctx, func(c Conn) error {
		found = nil
		closed, err := endedByCompression(ctx, c, parentSessionID)
		if err != nil || !closed {
			return err
		}
		found, err = querySessions(c.QueryContext(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved "+
			"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash = s.system_prompt_hash "+
			"WHERE s.parent_session_id = ? AND s.ended_at IS NULL\n"+nonContinuationChildSQL("s.", "?")+"ORDER BY s.started_at ASC LIMIT 2",
			parentSessionID, parentSessionID, parentSessionID, parentSessionID))
		return err
	})
	if err != nil || len(found) != 1 {
		return Session{}, false, err
	}
	return found[0], true, nil
}

func (s *Store) ReopenOrphanedCompressionSession(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	reopened := false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		reopened = false
		closed, err := endedByCompression(ctx, c, sessionID)
		if err != nil || !closed {
			return err
		}
		var one int
		err = c.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE parent_session_id = ?\n"+nonContinuationChildFilter+" LIMIT 1",
			sessionID, sessionID, sessionID, sessionID).Scan(&one)
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := nowEpoch()
		var holder sql.NullString
		var expires any
		err = c.QueryRowContext(ctx, compressionLockRowSQL, sessionID).Scan(&holder, &expires)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			at, numeric := cellFloat(expires)
			if expires == nil || (numeric && at >= now) {
				return nil
			}
			n, err := rowsChanged(c.ExecContext(ctx, "DELETE FROM compression_locks WHERE session_id = ? AND holder = ? AND expires_at = ?",
				sessionID, holder, expires))
			if err != nil || n != 1 {
				return err
			}
		}
		n, err := rowsChanged(c.ExecContext(ctx, "UPDATE sessions SET ended_at = NULL, end_reason = NULL "+
			"WHERE id = ? AND ended_at IS NOT NULL AND end_reason = 'compression'", sessionID))
		reopened = n == 1
		return err
	})
	return reopened, err
}

type publishParent struct {
	endedAt, endReason, cwd, gitBranch, gitRepoRoot, userID, sessionKey, chatID, chatType, threadID,
	displayName, originJSON, profileName, toolNames, archived, autoArchived, pinned, modelConfig, createdSource any
}

func readPublishParent(ctx context.Context, c Conn, sessionID string) (*publishParent, error) {
	var p publishParent
	err := c.QueryRowContext(ctx, `SELECT ended_at, end_reason, cwd, git_branch, git_repo_root,
                      user_id, session_key, chat_id, chat_type,
                      thread_id, display_name, origin_json, profile_name, tool_names,
                      archived, auto_archived, pinned, model_config, created_source
               FROM sessions WHERE id = ?`, sessionID).Scan(&p.endedAt, &p.endReason, &p.cwd, &p.gitBranch, &p.gitRepoRoot,
		&p.userID, &p.sessionKey, &p.chatID, &p.chatType, &p.threadID, &p.displayName, &p.originJSON, &p.profileName,
		&p.toolNames, &p.archived, &p.autoArchived, &p.pinned, &p.modelConfig, &p.createdSource)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

type childRow struct {
	parentID, childID, source, model, cwd, profileName string
	modelConfig                                        []configMember
	systemPrompt                                       *string
}

func orNil(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func intOrZero(v any) any {
	if !cellTruthy(v) {
		return int64(0)
	}
	return v
}

func (s *Store) publishChildSessionRow(ctx context.Context, c Conn, parent *publishParent, r childRow) error {
	_, branched := memberValue(r.modelConfig, "_branched_from")
	_, reset := memberValue(r.modelConfig, "_reset_from")
	continuation := !branched && !reset
	var merged []configMember
	if continuation {
		for _, m := range configMembers(textCell(parent.modelConfig)) {
			if containsString(conversationModelConfigKeys, m.key) {
				merged = setConfigMember(merged, m.key, m.value)
			}
		}
	}
	for _, m := range r.modelConfig {
		merged = setConfigMember(merged, m.key, m.value)
	}
	var config any
	if len(merged) > 0 {
		text, err := spacedJSON(rawObject(merged))
		if err != nil {
			return err
		}
		config = text
	}
	hash, err := storeSystemPrompt(ctx, c, r.systemPrompt)
	if err != nil {
		return err
	}
	created := createdSurfaceOf(textCell(parent.createdSource))
	if created == "" {
		created = createdSurfaceOf(r.source)
	}
	var cwd any = r.cwd
	if r.cwd == "" {
		cwd = parent.cwd
	}
	var profile any = r.profileName
	if r.profileName == "" {
		profile = parent.profileName
		if !cellTruthy(profile) {
			profile = orNil(s.OwnProfileName())
		}
	}
	var pinned, archived, autoArchived any = int64(0), int64(0), int64(0)
	if continuation {
		if n, ok := cellInt(parent.pinned); ok && cellTruthy(parent.pinned) {
			pinned = n
		}
		archived, autoArchived = intOrZero(parent.archived), intOrZero(parent.autoArchived)
	}
	_, err = c.ExecContext(ctx, `INSERT INTO sessions (
                   id, source, created_source, model, model_config, system_prompt,
                   system_prompt_hash, tool_names,
                   parent_session_id, cwd, git_branch, git_repo_root,
                   profile_name, user_id, session_key, chat_id, chat_type,
                   thread_id, display_name, origin_json, pinned, started_at,
                   archived, auto_archived
                ) VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.childID, r.source, orNil(created), orNil(r.model), config, hash, parent.toolNames, r.parentID, cwd,
		parent.gitBranch, parent.gitRepoRoot, profile, parent.userID, parent.sessionKey, parent.chatID, parent.chatType,
		parent.threadID, parent.displayName, parent.originJSON, pinned, nowEpoch(), archived, autoArchived)
	return err
}

type CompressionChild struct {
	ParentSessionID       string
	ChildSessionID        string
	Source                string
	Messages              []*Message
	Model                 string
	ModelConfig           json.RawMessage
	SystemPrompt          *string
	Cwd                   string
	ProfileName           string
	CompressionLockHolder string
	NoLeaseRequired       bool
	RequireLeaseRefresh   bool
	LeaseTTL              time.Duration
	Watermark             *int64
	WatermarkCeiling      *int64
}

func (s *Store) PublishCompressionChild(ctx context.Context, p CompressionChild) error {
	return s.transcriptWrite(ctx, WritePatience, p.Messages, func(c Conn) error {
		return s.PublishCompressionChildIn(ctx, c, p)
	})
}

func modelConfigMembers(raw json.RawMessage) ([]configMember, error) {
	if len(raw) == 0 || jsonIsNull(raw) {
		return nil, nil
	}
	members, ok := objectMembers(raw)
	if !ok {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	return members, nil
}

func (s *Store) PublishCompressionChildIn(ctx context.Context, c Conn, p CompressionChild) error {
	config, err := modelConfigMembers(p.ModelConfig)
	if err != nil {
		return err
	}
	ttl := p.LeaseTTL
	if ttl == 0 {
		ttl = DefaultCompressionLockTTL
	}
	if p.RequireLeaseRefresh && p.CompressionLockHolder != "" {
		if _, err := c.ExecContext(ctx, "UPDATE compression_locks SET expires_at = ? WHERE session_id = ? AND holder = ?",
			nowEpoch()+seconds(ttl), p.ParentSessionID, p.CompressionLockHolder); err != nil {
			return err
		}
	}
	lock, err := readLeaseRow(ctx, c, compressionLockRowSQL, p.ParentSessionID)
	if err != nil {
		return err
	}
	if !p.NoLeaseRequired && (lock == nil || p.CompressionLockHolder == "" || lock.holder != p.CompressionLockHolder || lock.expires <= nowEpoch()) {
		return &CompressionBusyError{msg: "Compression lease lost before publication: " + p.ParentSessionID}
	}
	parent, err := readPublishParent(ctx, c, p.ParentSessionID)
	if err != nil {
		return err
	}
	if parent == nil {
		return &CompressionPublishError{"Compression parent not found: " + p.ParentSessionID}
	}
	if parent.endedAt != nil {
		if !IsAutomaticEndReason(textCell(parent.endReason)) {
			return &CompressionPublishError{"Compression parent already ended: " + p.ParentSessionID}
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ?", p.ParentSessionID); err != nil {
			return err
		}
	}
	if len(p.Messages) == 0 {
		return &CompressionPublishError{"Compression child handoff must not be empty"}
	}
	if err := s.publishChildSessionRow(ctx, c, parent, childRow{parentID: p.ParentSessionID, childID: p.ChildSessionID, source: p.Source,
		model: p.Model, modelConfig: config, systemPrompt: p.SystemPrompt, cwd: p.Cwd, profileName: p.ProfileName}); err != nil {
		return err
	}
	s.carryParentTimestamps(ctx, c, p.ParentSessionID, p.Messages)
	totalMessages, totalToolCalls, err := s.insertMessageRows(ctx, c, p.ChildSessionID, p.Messages, true)
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
		ids, toolCalls, err := tailRows(ctx, c, query+" ORDER BY id", args...)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			if err := s.cloneMessageRows(ctx, c, ids, p.ChildSessionID); err != nil {
				return err
			}
			totalMessages += len(ids)
			totalToolCalls += toolCalls
		}
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET message_count = ?, tool_call_count = ? WHERE id = ?",
		totalMessages, totalToolCalls, p.ChildSessionID); err != nil {
		return err
	}
	n, err := rowsChanged(c.ExecContext(ctx, "UPDATE sessions SET ended_at = ?, end_reason = 'compression' WHERE id = ? AND ended_at IS NULL",
		nowEpoch(), p.ParentSessionID))
	if err != nil {
		return err
	}
	if n != 1 {
		return &CompressionPublishError{"Compression parent changed during publication: " + p.ParentSessionID}
	}
	if err := stampCompressionAncestors(ctx, c, p.ChildSessionID); err != nil {
		return err
	}
	if err := advanceLocalTarget(ctx, c, p.ParentSessionID, p.ChildSessionID, nil); err != nil {
		return err
	}
	if cellTruthy(parent.archived) {
		if _, err := unarchiveAutoArchivedLineage(ctx, c, p.ChildSessionID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeLogged(ctx context.Context, op, sessionID, query string, args ...any) error {
	err := s.WriteSQL(ctx, WritePatience, query, args...)
	if err != nil && familyOf(err) != notSQLite {
		slog.Warn(fmt.Sprintf("%s(%s) failed: %s", op, sessionID, errText(err)))
		return nil
	}
	return err
}

func (s *Store) RecordCompressionFailureCooldown(ctx context.Context, sessionID string, cooldownUntil float64, failure sql.Null[string]) error {
	if sessionID == "" {
		return nil
	}
	return s.writeLogged(ctx, "record_compression_failure_cooldown", sessionID,
		"UPDATE sessions SET compression_failure_cooldown_until = CASE "+
			"WHEN compression_failure_cooldown_until IS NOT NULL  AND compression_failure_cooldown_until > ? "+
			"THEN compression_failure_cooldown_until ELSE ? END, compression_failure_error = ? WHERE id = ?",
		cooldownUntil, cooldownUntil, nullText(failure), sessionID)
}

type CompressionCooldown struct {
	Until     float64
	Remaining float64
	Error     sql.Null[string]
}

type CooldownRow struct {
	Exists bool
	Until  sql.Null[float64]
	Error  sql.Null[string]
}

func (r CooldownRow) String() string {
	until, failure := "null", "null"
	if r.Until.Valid {
		until = strconv.FormatFloat(r.Until.V, 'g', -1, 64)
	}
	if r.Error.Valid {
		failure = strconv.Quote(r.Error.V)
	}
	return fmt.Sprintf("{session_exists: %t, cooldown_until: %s, error: %s}", r.Exists, until, failure)
}

func (s *Store) readCooldownRow(ctx context.Context, sessionID string) (CooldownRow, error) {
	var row CooldownRow
	if sessionID == "" {
		return row, nil
	}
	err := s.Read(ctx, func(c Conn) error {
		row = CooldownRow{}
		err := c.QueryRowContext(ctx, cooldownRowSQL, sessionID).Scan(&row.Until, &row.Error)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		row.Exists = err == nil
		return err
	})
	return row, err
}

func (s *Store) GetCompressionFailureCooldown(ctx context.Context, sessionID string) (CompressionCooldown, bool, error) {
	now := nowEpoch()
	row, err := s.readCooldownRow(ctx, sessionID)
	if err != nil || !row.Exists || !row.Until.Valid || row.Until.V <= now {
		return CompressionCooldown{}, false, err
	}
	return CompressionCooldown{Until: row.Until.V, Remaining: row.Until.V - now, Error: row.Error}, true, nil
}

func (s *Store) GetCompressionFailureCooldownRow(ctx context.Context, sessionID string) (CooldownRow, error) {
	return s.readCooldownRow(ctx, sessionID)
}

func (s *Store) RestoreCompressionFailureCooldownRow(ctx context.Context, sessionID string, snapshot CooldownRow) error {
	if !snapshot.Exists {
		current, err := s.readCooldownRow(ctx, sessionID)
		if err != nil {
			return err
		}
		if current.Exists {
			return errors.New("cannot restore absent compression cooldown row: session now exists")
		}
		return nil
	}
	n, err := s.WriteRowcount(ctx, WritePatience, "UPDATE sessions SET compression_failure_cooldown_until = ?, "+
		"compression_failure_error = ? WHERE id = ?", nullFloat(snapshot.Until), nullText(snapshot.Error), sessionID)
	if err != nil {
		return err
	}
	if n != 1 {
		slog.Warn("compression cooldown rollback session missing: " + sessionID)
		return nil
	}
	actual, err := s.readCooldownRow(ctx, sessionID)
	if err != nil {
		return err
	}
	expected := CooldownRow{Exists: true, Until: snapshot.Until, Error: snapshot.Error}
	if actual != expected {
		if !actual.Exists {
			slog.Warn("compression cooldown rollback session missing after restore: " + sessionID)
			return nil
		}
		return fmt.Errorf("compression cooldown rollback verification failed: expected=%s, actual=%s", expected, actual)
	}
	return nil
}

func nullFloat(v sql.Null[float64]) any {
	if !v.Valid {
		return nil
	}
	return v.V
}

func (s *Store) ClearCompressionFailureCooldown(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	return s.writeLogged(ctx, "clear_compression_failure_cooldown", sessionID,
		"UPDATE sessions SET compression_failure_cooldown_until = NULL, compression_failure_error = NULL WHERE id = ?", sessionID)
}

func numericCell(v any, integer bool) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case float64:
		if integer {
			return float64(int64(t)), true
		}
		return t, true
	case string, []byte:
		text := strings.TrimSpace(cellString(t))
		if integer {
			n, err := strconv.ParseInt(text, 10, 64)
			return float64(n), err == nil
		}
		f, err := strconv.ParseFloat(text, 64)
		return f, err == nil
	}
	return 0, false
}

func cellString(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	s, _ := v.(string)
	return s
}

func (s *Store) readSessionNumber(ctx context.Context, column, sessionID string, integer bool) (float64, error) {
	if sessionID == "" {
		return 0, nil
	}
	var cell any
	err := s.Read(ctx, func(c Conn) error {
		cell = nil
		err := c.QueryRowContext(ctx, "SELECT "+column+" FROM sessions WHERE id = ?", sessionID).Scan(&cell)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	n, ok := numericCell(cell, integer)
	if !ok {
		return 0, nil
	}
	return max(0, n), nil
}

func (s *Store) readSessionCount(ctx context.Context, column, sessionID string) (int64, error) {
	n, err := s.readSessionNumber(ctx, column, sessionID, true)
	return int64(n), err
}

func (s *Store) writeSessionColumn(ctx context.Context, column, sessionID string, value any) error {
	return s.WriteSQL(ctx, WritePatience, "UPDATE sessions SET "+column+" = ? WHERE id = ?", value, sessionID)
}

func (s *Store) GetCompressionFallbackStreak(ctx context.Context, sessionID string) (int64, error) {
	return s.readSessionCount(ctx, "compression_fallback_streak", sessionID)
}

func (s *Store) SetCompressionFallbackStreak(ctx context.Context, sessionID string, streak int64) error {
	if sessionID == "" {
		return nil
	}
	return s.writeSessionColumn(ctx, "compression_fallback_streak", sessionID, max(0, streak))
}

func (s *Store) GetCompressionIneffectiveCount(ctx context.Context, sessionID string) (int64, error) {
	return s.readSessionCount(ctx, "compression_ineffective_count", sessionID)
}

func (s *Store) SetCompressionIneffectiveCount(ctx context.Context, sessionID string, count int64) error {
	if sessionID == "" {
		return nil
	}
	return s.writeSessionColumn(ctx, "compression_ineffective_count", sessionID, max(0, count))
}

func (s *Store) GetCompressionRecoveryDeadline(ctx context.Context, sessionID string) (float64, error) {
	return s.readSessionNumber(ctx, "compression_recovery_deadline", sessionID, false)
}

func (s *Store) SetCompressionRecoveryDeadline(ctx context.Context, sessionID string, deadline float64) error {
	if sessionID == "" {
		return nil
	}
	var value any
	if normalized := max(0, deadline); normalized != 0 {
		value = normalized
	}
	return s.writeSessionColumn(ctx, "compression_recovery_deadline", sessionID, value)
}

func (s *Store) GetCompressionOverloadStreak(ctx context.Context, sessionID string) (int64, error) {
	return s.readSessionCount(ctx, "compression_overload_streak", sessionID)
}

func (s *Store) SetCompressionOverloadStreak(ctx context.Context, sessionID string, streak int64) error {
	if sessionID == "" {
		return nil
	}
	return s.writeSessionColumn(ctx, "compression_overload_streak", sessionID, max(0, streak))
}

func (s *Store) IncrementCompressionOverloadStreak(ctx context.Context, sessionID string) (int64, bool, error) {
	if sessionID == "" {
		return 0, false, nil
	}
	var streak sql.Null[int64]
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		streak = sql.Null[int64]{}
		err := c.QueryRowContext(ctx, "UPDATE sessions SET compression_overload_streak = compression_overload_streak + 1"+
			" WHERE id = ? RETURNING compression_overload_streak", sessionID).Scan(&streak)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return streak.V, streak.Valid, err
}

func (s *Store) RefreshCompressionLock(ctx context.Context, sessionID, holder string, ttl time.Duration) (bool, error) {
	if sessionID == "" || holder == "" {
		return false, nil
	}
	n, err := s.WriteRowcount(ctx, WritePatience, "UPDATE compression_locks SET expires_at = ? WHERE session_id = ? AND holder = ?",
		nowEpoch()+seconds(ttl), sessionID, holder)
	if err != nil && familyOf(err) != notSQLite {
		slog.Warn(fmt.Sprintf("refresh_compression_lock(%s) failed: %s", sessionID, errText(err)))
		return false, nil
	}
	return n > 0, err
}

func claimLeaseRow(ctx context.Context, c Conn, table, keyCol, key, holder string, now, expires float64,
	stale func(holder string, expires float64) bool) (bool, string, error) {
	reclaimed := ""
	row, err := readLeaseRow(ctx, c, "SELECT holder, expires_at FROM "+table+" WHERE "+keyCol+" = ?", key)
	if err != nil {
		return false, "", err
	}
	if row != nil && stale(row.holder, row.expires) {
		if _, err := c.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+keyCol+" = ? AND holder = ?", key, row.holder); err != nil {
			return false, "", err
		}
		reclaimed = row.holder
	}
	if _, err := c.ExecContext(ctx, "INSERT OR IGNORE INTO "+table+" ("+keyCol+", holder, acquired_at, expires_at) VALUES (?, ?, ?, ?)",
		key, holder, now, expires); err != nil {
		return false, "", err
	}
	owners, err := queryStrings(ctx, c, "SELECT holder FROM "+table+" WHERE "+keyCol+" = ?", key)
	if err != nil {
		return false, "", err
	}
	return len(owners) > 0 && owners[0] == holder, reclaimed, nil
}

func (s *Store) TryAcquireCompressionLock(ctx context.Context, sessionID, holder string, ttl time.Duration) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	now := nowEpoch()
	expires := now + seconds(ttl)
	acquired, reclaimed := false, ""
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var err error
		acquired, reclaimed, err = claimLeaseRow(ctx, c, "compression_locks", "session_id", sessionID, holder, now, expires,
			func(h string, e float64) bool { return e < now || s.eng.holderProcessGone(h) })
		return err
	})
	if err != nil {
		if familyOf(err) != notSQLite {
			slog.Warn(fmt.Sprintf("try_acquire_compression_lock(%s) failed: %s", sessionID, errText(err)))
			return false, nil
		}
		return false, err
	}
	if reclaimed != "" {
		slog.Warn(fmt.Sprintf("Reclaimed stale compression lock for session=%s (holder=%s)", sessionID, reclaimed))
	}
	return acquired, nil
}

func (s *Store) ReleaseCompressionLock(ctx context.Context, sessionID, holder string) error {
	if sessionID == "" {
		return nil
	}
	return s.writeLogged(ctx, "release_compression_lock", sessionID,
		"DELETE FROM compression_locks WHERE session_id = ? AND holder = ?", sessionID, holder)
}

func (s *Store) TryAcquireSessionTurnLease(ctx context.Context, sessionID, holder string, ttl, patience time.Duration) (bool, error) {
	if sessionID == "" || holder == "" {
		return false, nil
	}
	if patience <= 0 {
		patience = WritePatience
	}
	now := nowEpoch()
	expires := now + max(0.1, seconds(ttl))
	acquired := false
	err := s.Write(ctx, patience, func(c Conn) error {
		if _, err := c.ExecContext(ctx, "DELETE FROM session_turn_leases WHERE expires_at < ?", now-turnLeaseSweepGrace); err != nil {
			return err
		}
		key, err := sessionTurnLeaseKeyIn(ctx, c, sessionID)
		if err != nil {
			return err
		}
		acquired, _, err = claimLeaseRow(ctx, c, "session_turn_leases", "conversation_id", key, holder, now, expires,
			func(h string, e float64) bool { return e <= now || s.eng.holderProcessGone(h) })
		return err
	})
	return acquired, err
}

type TurnLeaseWait struct {
	TTL         time.Duration
	Wait        time.Duration
	OnWait      func(elapsed time.Duration)
	OnContended func()
	ShouldAbort func() bool
}

func (s *Store) AcquireSessionTurnLease(ctx context.Context, sessionID, holder string, o TurnLeaseWait) (bool, error) {
	deadline := time.Now().Add(max(0, o.Wait))
	patience := turnLeaseAcquirePatience
	var waitStarted, lastNotice time.Time
	for {
		if o.ShouldAbort != nil && o.ShouldAbort() {
			return false, nil
		}
		acquired, err := s.TryAcquireSessionTurnLease(ctx, sessionID, holder, o.TTL, patience)
		if err != nil {
			if Classify(err) != CauseLocked {
				return false, err
			}
			if o.OnContended != nil {
				o.OnContended()
			}
			patience = min(turnLeaseAcquirePatience+turnLeasePollInterval, max(turnLeaseAcquirePatience, time.Until(deadline)))
			retry, err := sleepBeforeRetry(ctx, deadline, 0)
			if err != nil || !retry {
				return false, err
			}
			continue
		}
		if acquired {
			return true, nil
		}
		patience = turnLeaseAcquirePatience
		now := time.Now()
		remaining := deadline.Sub(now)
		if remaining <= 0 {
			return false, nil
		}
		if waitStarted.IsZero() {
			waitStarted = now
		}
		if o.OnWait != nil && (lastNotice.IsZero() || now.Sub(lastNotice) >= turnLeaseNoticeInterval) {
			o.OnWait(max(0, now.Sub(waitStarted)))
			lastNotice = now
		}
		if err := sleepCtx(ctx, min(turnLeasePollInterval, remaining)); err != nil {
			return false, err
		}
	}
}

func (s *Store) SessionTurnLeaseExpiresAt(ctx context.Context, sessionID, holder string) (float64, bool, error) {
	if sessionID == "" || holder == "" {
		return 0, false, nil
	}
	var expires sql.Null[float64]
	err := s.Read(ctx, func(c Conn) error {
		expires = sql.Null[float64]{}
		key, err := sessionTurnLeaseKeyIn(ctx, c, sessionID)
		if err != nil {
			return err
		}
		err = c.QueryRowContext(ctx, "SELECT expires_at FROM session_turn_leases WHERE conversation_id = ? AND holder = ?", key, holder).Scan(&expires)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return expires.V, expires.Valid, err
}

func (s *Store) RefreshSessionTurnLease(ctx context.Context, sessionID, holder string, ttl, patience time.Duration) (bool, error) {
	if sessionID == "" || holder == "" {
		return false, nil
	}
	if patience <= 0 {
		patience = WritePatience
	}
	expires := nowEpoch() + max(0.1, seconds(ttl))
	refreshed := false
	err := s.Write(ctx, patience, func(c Conn) error {
		key, err := sessionTurnLeaseKeyIn(ctx, c, sessionID)
		if err != nil {
			return err
		}
		n, err := rowsChanged(c.ExecContext(ctx, "UPDATE session_turn_leases SET expires_at = ? WHERE conversation_id = ? AND holder = ?",
			expires, key, holder))
		refreshed = n > 0
		return err
	})
	return refreshed, err
}

func (s *Store) ReleaseSessionTurnLease(ctx context.Context, sessionID, holder string) error {
	if sessionID == "" || holder == "" {
		return nil
	}
	return s.Write(ctx, WritePatience, func(c Conn) error {
		key, err := sessionTurnLeaseKeyIn(ctx, c, sessionID)
		if err != nil {
			return err
		}
		_, err = c.ExecContext(ctx, "DELETE FROM session_turn_leases WHERE conversation_id = ? AND holder = ?", key, holder)
		return err
	})
}

func (s *Store) GetCompressionLockHolder(ctx context.Context, sessionID string) (string, bool, error) {
	if sessionID == "" {
		return "", false, nil
	}
	return s.readText(ctx, "SELECT holder FROM compression_locks WHERE session_id = ? AND expires_at >= ?", sessionID, nowEpoch())
}

func (s *Store) FinalizeOrphanedCompressionSessions(ctx context.Context) (int64, error) {
	cutoff := nowEpoch() - orphanedCompressionAge
	return s.WriteRowcount(ctx, WritePatience, `
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
                `, nowEpoch(), cutoff)
}
