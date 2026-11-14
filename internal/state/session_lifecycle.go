package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// Exact title Bot Mode resolves its canonical chat by, the title being that chat's identity
const CanonicalBotChatTitle = "Bot Chat"

// Increment of a routing peer's conversation generation, the first boundary writing one
const bumpGenerationSQL = `
            INSERT INTO conversation_generations (source, session_key, generation)
            VALUES (?, ?, 1)
            ON CONFLICT(source, session_key) DO UPDATE
                SET generation = conversation_generations.generation + 1
            `

// Marks a session ended, the first end reason winning until a reopen
func (s *Store) EndSession(ctx context.Context, sessionID, endReason string) error {
	return s.Write(ctx, WritePatience, func(c Conn) error {
		_, err := EndAndBumpIn(ctx, c, "UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL",
			[]any{nowEpoch(), endReason, sessionID}, sessionID, endReason)
		return err
	})
}

// Runs an end stamp and advances the peer's conversation generation only when the stamp was written
func EndAndBumpIn(ctx context.Context, c Conn, query string, args []any, sessionID, reason string) (int64, error) {
	res, err := c.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed > 0 {
		if err := bumpConversationGeneration(ctx, c, sessionID, reason); err != nil {
			return 0, err
		}
	}
	return changed, nil
}

// Advances the routing peer's generation past a reset boundary inside the transaction writing it
func bumpConversationGeneration(ctx context.Context, c Conn, sessionID, endReason string) error {
	if !IsResetEndReason(endReason) {
		return nil
	}
	var source, key sql.NullString
	err := c.QueryRowContext(ctx, "SELECT source, session_key FROM sessions WHERE id = ?", sessionID).Scan(&source, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	src, k := strings.TrimSpace(source.String), strings.TrimSpace(key.String)
	if src == "" || k == "" {
		return nil
	}
	_, err = c.ExecContext(ctx, bumpGenerationSQL, src, k)
	return err
}

// Deactivates busy queue accept rows a restart left undrained on c, returning rows retired
func retireUndrainedQueueRows(ctx context.Context, c Conn, sessionID string) (int64, error) {
	res, err := c.ExecContext(ctx, "UPDATE messages SET active = 0 "+
		"WHERE session_id = ? AND role = 'user' AND active = 1 "+
		"AND COALESCE("+JSONExtractSQL("display_metadata", "$."+QueuedPromptMetadataKey)+", 0) = 1", sessionID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Deactivates never drained busy queue accept rows without clearing ended_at, returning rows retired
func (s *Store) RetireUndrainedQueueRows(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var err error
		n, err = retireUndrainedQueueRows(ctx, c, sessionID)
		return err
	})
	return n, err
}

// Clears the end stamp so a session resumes, freezing legacy reset children and lifting an idle archive first
func (s *Store) ReopenSession(ctx context.Context, sessionID string) error {
	return s.Write(ctx, WritePatience, func(c Conn) error {
		if _, err := retireUndrainedQueueRows(ctx, c, sessionID); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions AS child SET model_config = json_set("+
			"COALESCE(child.model_config, '{}'), '$._reset_from', child.parent_session_id) "+
			"WHERE child.parent_session_id = ? AND "+JSONExtractSQL("child.model_config", "$._reset_from")+" IS NULL "+
			"AND "+JSONExtractSQL("child.model_config", "$._branched_from")+" IS NULL "+
			"AND "+JSONExtractSQL("child.model_config", "$._delegate_from")+" IS NULL "+
			"AND COALESCE(child.source, '') != 'tool' "+
			"AND child.started_at >= (SELECT p.started_at FROM sessions p WHERE p.id = child.parent_session_id) "+
			"AND "+legacyResetChildSQL("child", bindMarks(len(resetEndReasons))),
			append([]any{sessionID}, stringArgs(resetEndReasons)...)...); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ?", sessionID); err != nil {
			return err
		}
		_, err := unarchiveAutoArchivedLineage(ctx, c, sessionID)
		return err
	})
}

// Marks a deliberate reset boundary on a live row or one ended by a recoverable accident, true when written
func (s *Store) PromoteToSessionReset(ctx context.Context, sessionID, reason string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	if reason == "" {
		reason = "session_reset"
	}
	var changed int64
	now := nowEpoch()
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var err error
		changed, err = EndAndBumpIn(ctx, c, "UPDATE sessions SET ended_at = ?, end_reason = ? WHERE id = ? AND (ended_at IS NULL "+
			"OR end_reason IN ("+sqlLiteralList(recoverableEndReasons)+"))", []any{now, reason, sessionID}, sessionID, reason)
		return err
	})
	return changed > 0, err
}

// Sets one sessions column across a whole compression lineage on c, returning the ids it touched
func setLineageColumnIn(ctx context.Context, c Conn, column, sessionID string, value any, extraSetSQL string) ([]string, error) {
	if column == "archived" && extraSetSQL == "" {
		extraSetSQL = ", auto_archived = 0"
	}
	return queryStrings(ctx, c, lineageCTESQL+`
            UPDATE sessions
            SET `+column+` = ?`+extraSetSQL+`
            WHERE id IN (SELECT id FROM lineage)
            RETURNING id
            `, sessionID, sessionID, value)
}

// Sets one sessions column across a compression lineage, true when any row changed
func (s *Store) setLineageColumn(ctx context.Context, column, sessionID string, value any, extraSetSQL string) (bool, error) {
	var ids []string
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var err error
		ids, err = setLineageColumnIn(ctx, c, column, sessionID, value, extraSetSQL)
		return err
	})
	return len(ids) > 0, err
}

// Soft hides or unhides a lineage as a deliberate archive, a real archive flip firing the archive event after commit
func (s *Store) SetSessionArchived(ctx context.Context, sessionID string, archived, notify bool) (bool, error) {
	var ids []string
	newly := false
	err := s.Write(ctx, WritePatience, func(c Conn) error {
		var before sql.NullInt64
		found := true
		err := c.QueryRowContext(ctx, "SELECT archived FROM sessions WHERE id = ?", sessionID).Scan(&before)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
		} else if err != nil {
			return err
		}
		ids, err = setLineageColumnIn(ctx, c, "archived", sessionID, boolInt(archived), ", auto_archived = 0")
		newly = archived && len(ids) > 0 && found && before.Int64 == 0
		return err
	})
	if err != nil {
		return false, err
	}
	if newly && notify {
		s.emitSessionDeleted(ctx, ids, DeleteReasonArchive, sessionID, "")
	}
	return len(ids) > 0, nil
}

// Idle sweep archive that stamps auto_archived only on rows it hides
func (s *Store) autoArchiveLineage(ctx context.Context, sessionID string) (bool, error) {
	return s.setLineageColumn(ctx, "archived", sessionID, 1, ", auto_archived = CASE WHEN archived = 0 THEN 1 ELSE auto_archived END")
}

// Un-hides a lineage the idle sweep archived inside the caller's transaction, a deliberate archive anywhere leaving it alone
func unarchiveAutoArchivedLineage(ctx context.Context, c Conn, sessionID string) (bool, error) {
	var one int
	err := c.QueryRowContext(ctx, lineageCTESQL+`
            SELECT 1 FROM sessions
            WHERE id IN (SELECT id FROM lineage) AND archived <> 0 AND COALESCE(auto_archived, 0) = 0
            LIMIT 1
            `, sessionID, sessionID).Scan(&one)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	res, err := c.ExecContext(ctx, lineageCTESQL+`
            UPDATE sessions SET archived = 0, auto_archived = 0
            WHERE id IN (SELECT id FROM lineage) AND archived <> 0
            `, sessionID, sessionID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Un-archives a session a recoverable accident archived and clears that accidental end stamp, true when un-archived
func (s *Store) UnarchiveRecoverableSession(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	row, ok, err := s.GetSession(ctx, sessionID)
	if err != nil || !ok || row.Archived == 0 {
		return false, err
	}
	tip := row
	if t, ok, err := s.recoverableTip(ctx, sessionID); err != nil {
		slog.Debug(fmt.Sprintf("compression tip of %s unreadable, judging recoverability on the row itself: %s", sessionID, errText(err)))
	} else if ok {
		tip = t
	}
	if !IsRecoverableEndReason(tip.EndReason.V) {
		return false, nil
	}
	if done, err := s.SetSessionArchived(ctx, sessionID, false, true); err != nil || !done {
		return false, err
	}
	if err := s.WriteSQL(ctx, WritePatience, "UPDATE sessions SET ended_at = NULL, end_reason = NULL WHERE id = ?", tip.ID); err != nil {
		return false, err
	}
	return true, nil
}

// Row of the live compression tip, false when the session is its own tip
func (s *Store) recoverableTip(ctx context.Context, sessionID string) (Session, bool, error) {
	tipID, err := s.CompressionTip(ctx, sessionID)
	if err != nil || tipID == "" || tipID == sessionID {
		return Session{}, false, err
	}
	return s.GetSession(ctx, tipID)
}

// Pins or unpins a lineage, a pin also unhiding it unless the row is the hidden canonical Bot Chat
func (s *Store) SetSessionPinned(ctx context.Context, sessionID string, pinned bool) (bool, error) {
	result, err := s.setLineageColumn(ctx, "pinned", sessionID, boolInt(pinned), "")
	if err != nil || !pinned {
		return result, err
	}
	row, ok, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return result, err
	}
	if !(ok && row.Hidden != 0 && row.Title.V == CanonicalBotChatTitle) {
		if _, err := s.setLineageColumn(ctx, "hidden", sessionID, 0, ""); err != nil {
			return result, err
		}
	}
	return result, nil
}

// Hides or unhides a lineage from the default listing, the session staying resumable
func (s *Store) SetSessionHidden(ctx context.Context, sessionID string, hidden bool) (bool, error) {
	return s.setLineageColumn(ctx, "hidden", sessionID, boolInt(hidden), "")
}

// Marks a lineage read now or unread with a zero watermark
func (s *Store) SetSessionRead(ctx context.Context, sessionID string, read bool) (bool, error) {
	watermark := 0.0
	if read {
		watermark = nowEpoch()
	}
	return s.setLineageColumn(ctx, "last_read_at", sessionID, watermark, "")
}
