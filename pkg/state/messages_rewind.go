package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Keyword arguments of a rewind, nil standing for an omitted Python argument
type RewindOptions struct {
	PreserveCompactionHandoff bool
	ExpectedActiveIDs         []int64
	ExpectedTargetContent     any
}

// Validates an active user originated rewind target and returns its handoff scaffold when asked to keep it
func (d *SessionDB) splitRewindTarget(targetRow *pyjson.Object, expectedTargetContent any, preserveCompactionHandoff bool) (map[string]any, error) {
	if !pyTruthy(targetRow.Vals["active"]) {
		return nil, pyValueError("rewind target is not active")
	}
	view := objMap(targetRow)
	view["content"] = decodeMessageContent(targetRow.Vals["content"])
	view["display_metadata"] = decodeDisplayMetadata(targetRow.Vals["display_metadata"])
	handoff, liveView := SplitUserOriginatedTurn(view)
	if liveView == nil {
		return nil, pyValueError("rewind target is not a user-originated turn")
	}
	liveContent := liveView["content"]
	if text, ok := liveContent.(string); ok {
		liveContent = pyStrip(SanitizeContext(text))
	}
	if expectedTargetContent != nil && !pyrepr.Equal(liveContent, expectedTargetContent) {
		return nil, errors.New("rewind target changed before it could be persisted")
	}
	if preserveCompactionHandoff && handoff == nil {
		return nil, pyValueError("preserve_compaction_handoff requires an active composite carrier")
	}
	if preserveCompactionHandoff {
		return handoff, nil
	}
	return nil, nil
}

// Transaction scoped rewind returning the target row, the rewound ids, the new head id and the scaffold row id
func (d *SessionDB) rewindInTransaction(ctx context.Context, conn SQLiteConn, sessionID string, targetMessageID int64, o RewindOptions) (*pyjson.Object, []int64, any, *int64, error) {
	if err := d.checkTranscriptWriteGuards(ctx, conn, sessionID, transcriptGuards{rejectActiveTurnLease: true, rejectActiveCompressionLock: true}); err != nil {
		return nil, nil, nil, nil, err
	}
	if o.ExpectedActiveIDs != nil {
		active, err := connRows(ctx, conn, activeIDsSQL, sessionID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if !slices.Equal(rowIDList(active), o.ExpectedActiveIDs) {
			return nil, nil, nil, nil, errors.New("active transcript changed before the rewind could be persisted")
		}
	}
	rows, err := queryObjects(ctx, conn, "SELECT * FROM messages WHERE id = ? AND session_id = ?", targetMessageID, sessionID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if len(rows) == 0 {
		return nil, nil, nil, nil, pyValueError(fmt.Sprintf("message %d not found in session %s", targetMessageID, sessionID))
	}
	targetRow := rows[0]
	if role := targetRow.Vals["role"]; role != "user" {
		return nil, nil, nil, nil, pyValueError(fmt.Sprintf("rewind target must be a 'user' message (got role=%s, id=%d)", pyrepr.Repr(role), targetMessageID))
	}
	var replacement map[string]any
	if o.PreserveCompactionHandoff || o.ExpectedTargetContent != nil {
		if replacement, err = d.splitRewindTarget(targetRow, o.ExpectedTargetContent, o.PreserveCompactionHandoff); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	idRows, err := connRows(ctx, conn, "SELECT id FROM messages WHERE session_id = ? AND id >= ? AND active = 1", sessionID, targetMessageID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	ids := rowIDList(idRows)
	if len(ids) > 0 {
		if _, err := conn.ExecContext(ctx, "UPDATE messages SET active = 0 WHERE id IN ("+placeholders(len(ids))+")", int64sToAny(ids)...); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	var replacementID *int64
	if replacement != nil {
		if _, _, err := d.insertMessageRows(ctx, conn, sessionID, []map[string]any{replacement}, true); err != nil {
			return nil, nil, nil, nil, err
		}
		last, err := connRow(ctx, conn, "SELECT last_insert_rowid() AS id")
		if err != nil {
			return nil, nil, nil, nil, err
		}
		id, _ := pyIntID(last["id"])
		replacementID = &id
	}
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET rewind_count = COALESCE(rewind_count, 0) + 1 WHERE id = ?", sessionID); err != nil {
		return nil, nil, nil, nil, err
	}
	messageCount, toolCallCount, err := activeTranscriptCounts(ctx, conn, sessionID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, setCountersSQL+" WHERE id = ?", messageCount, toolCallCount, sessionID); err != nil {
		return nil, nil, nil, nil, err
	}
	head, err := connRow(ctx, conn, "SELECT MAX(id) AS head FROM messages WHERE session_id = ? AND active = 1", sessionID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return targetRow, ids, head["head"], replacementID, nil
}

// Soft deletes every active message from the target user row on and reports the rewound count, target and new head
func (d *SessionDB) RewindToMessage(ctx context.Context, sessionID string, targetMessageID int64, o RewindOptions) (*pyjson.Object, error) {
	var (
		targetRow      *pyjson.Object
		rewound        []int64
		newHeadID      any
		replacementID  *int64
		replacementUID any
	)
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		targetRow, rewound, newHeadID, replacementID, err = d.rewindInTransaction(ctx, conn, sessionID, targetMessageID, o)
		if err != nil {
			return err
		}
		replacementUID = nil
		if replacementID != nil {
			row, err := connRow(ctx, conn, "SELECT message_uid FROM messages WHERE id = ?", *replacementID)
			if err != nil {
				return err
			}
			replacementUID = row["message_uid"]
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	targetRow.Set("content", decodeMessageContent(targetRow.Vals["content"]))
	out := pyjson.NewObject()
	out.Set("rewound_count", len(rewound))
	out.Set("target_message", targetRow)
	out.Set("new_head_id", newHeadID)
	if o.PreserveCompactionHandoff {
		if replacementID != nil {
			out.Set("replacement_message_id", *replacementID)
		} else {
			out.Set("replacement_message_id", nil)
		}
		out.Set("replacement_message_uid", replacementUID)
	}
	return out, nil
}

// Clears bare tool call marker content left by older sessions, with an optional VACUUM INTO snapshot first
func (d *SessionDB) PurgeStaleToolCallMarkers(ctx context.Context, dryRun, backup bool) (*pyjson.Object, error) {
	findAffected := func(conn SQLiteConn) ([]int64, error) {
		rows, err := connRows(ctx, conn, "SELECT id, content FROM messages WHERE role = 'assistant' AND tool_calls IS NOT NULL AND tool_calls != ''")
		if err != nil {
			return nil, err
		}
		ids := []int64{}
		for _, row := range rows {
			if text, ok := row["content"].(string); ok && staleToolCallMarkerRe.MatchString(pyStrip(text)) {
				id, _ := pyIntID(row["id"])
				ids = append(ids, id)
			}
		}
		return ids, nil
	}
	result := func(affected []int64, backupPath any) *pyjson.Object {
		out := pyjson.NewObject()
		out.Set("dry_run", dryRun)
		out.Set("rows_affected", len(affected))
		out.Set("row_ids", int64sToAny(affected))
		out.Set("backup_path", backupPath)
		return out
	}
	var affected []int64
	if err := d.readCtx(ctx, true, func(conn SQLiteConn) error {
		var err error
		affected, err = findAffected(conn)
		return err
	}); err != nil {
		return nil, err
	}
	if dryRun || len(affected) == 0 {
		return result(affected, nil), nil
	}
	var backupPath any
	if backup {
		path := d.dbPath()
		target := filepath.Join(filepath.Dir(path), filepath.Base(path)+".pre-clean-markers-backup-"+time.Now().Format("20060102_150405"))
		d.mu.Lock()
		var err error
		if d.conn == nil {
			err = &SQLiteError{Class: SQLiteProgrammingError, Msg: "Cannot operate on a closed database."}
		} else {
			_, err = d.conn.ExecContext(ctx, "VACUUM INTO ?", target)
		}
		d.mu.Unlock()
		if err != nil {
			return nil, err
		}
		backupPath = target
		slog.Info(fmt.Sprintf("Backed up state.db to %s before clean-markers write", target))
	}
	if err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		ids, err := findAffected(conn)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			if _, err := conn.ExecContext(ctx, "UPDATE messages SET content = '' WHERE id IN ("+placeholders(len(ids))+")", int64sToAny(ids)...); err != nil {
				return err
			}
		}
		affected = ids
		return nil
	}, nil); err != nil {
		return nil, err
	}
	if len(affected) > 0 {
		slog.Info(fmt.Sprintf("Permanently cleared %d stale tool-call marker row(s) in state.db (#78148)", len(affected)))
	}
	return result(affected, backupPath), nil
}
