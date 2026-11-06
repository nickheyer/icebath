package state

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	liveIdentitySQL = "SELECT id, role, content, tool_call_id, tool_calls, message_uid FROM messages WHERE session_id = ? AND active = 1 ORDER BY id LIMIT ?"
	setCountersSQL  = "UPDATE sessions SET message_count = ?, tool_call_count = ?"
)

// Model only rows never enter a display projection, unqualified so a correlated subquery binds the inner alias
var DisplayVisibleSQL = " AND COALESCE(" + sqlJSONExtract("display_metadata", "$."+ModelOnlyKey) + ", 0) = 0"

// Modes of a transcript replacement
type ReplaceOptions struct {
	ActiveOnly            bool
	ArchiveDropped        bool
	RejectActiveTurnLease bool
}

// Atomically replaces a session's messages, deleting or soft archiving the dropped live rows
func (d *SessionDB) ReplaceMessages(ctx context.Context, sessionID string, messages []map[string]any, opts ReplaceOptions) error {
	return d.executeTranscriptWrite(ctx, func(conn SQLiteConn) error {
		return d.ReplaceMessagesInTransaction(ctx, conn, sessionID, messages, opts, nil)
	}, messages, nil)
}

// Replacement inside the caller transaction, refusing when the active ids moved off expectedActiveIDs
func (d *SessionDB) ReplaceMessagesInTransaction(ctx context.Context, conn SQLiteConn, sessionID string, messages []map[string]any, opts ReplaceOptions, expectedActiveIDs []int64) error {
	if opts.RejectActiveTurnLease {
		if err := d.checkTranscriptWriteGuards(ctx, conn, sessionID, transcriptGuards{rejectActiveTurnLease: true, rejectActiveCompressionLock: true}); err != nil {
			return err
		}
	} else {
		ended, err := connRow(ctx, conn, endedRowSQL, sessionID)
		if err != nil {
			return err
		}
		if endedByCompression(ended) {
			return &CompressionSessionClosedError{SessionID: sessionID}
		}
	}
	if expectedActiveIDs != nil {
		rows, err := connRows(ctx, conn, activeIDsSQL, sessionID)
		if err != nil {
			return err
		}
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i], _ = pyIntID(r["id"])
		}
		if !slices.Equal(ids, expectedActiveIDs) {
			return errors.New("active transcript changed before the rewrite could be persisted")
		}
	}
	kept, keptToolCalls := 0, 0
	if opts.ArchiveDropped {
		live, err := connRows(ctx, conn, liveIdentitySQL, sessionID, len(messages)+1)
		if err != nil {
			return err
		}
		if kept, err = stampKeptLivePrefix(live, messages); err != nil {
			return err
		}
		for _, row := range live[:kept] {
			keptToolCalls += toolCallsLen(row["tool_calls"], 1)
		}
		if kept < len(live) {
			if _, err := conn.ExecContext(ctx, "UPDATE messages SET active = 0 WHERE session_id = ? AND active = 1 AND id >= ?", sessionID, live[kept]["id"]); err != nil {
				return err
			}
		}
	} else {
		query := "DELETE FROM messages WHERE session_id = ?"
		if opts.ActiveOnly {
			query += " AND active = 1"
		}
		if _, err := conn.ExecContext(ctx, query, sessionID); err != nil {
			return err
		}
	}
	inserted, insertedToolCalls, err := d.insertMessageRows(ctx, conn, sessionID, messages[kept:], true)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, setCountersSQL+" WHERE id = ?", kept+inserted, keptToolCalls+insertedToolCalls, sessionID)
	return err
}

// Role, loaded view content, tool call id and encoded tool calls a kept prefix match compares
func rowIdentity(role, content, toolCallID, toolCalls any) (pyrepr.Tuple, error) {
	roleStr, _ := role.(string)
	var encodedCalls any
	if pyTruthy(toolCalls) {
		text, err := pyDumps(toolCalls, pyJSONDefault)
		if err != nil {
			return nil, err
		}
		encodedCalls = text
	}
	return pyrepr.Tuple{role, encodeMessageContent(loadedViewContent(roleStr, content)), toolCallID, encodedCalls}, nil
}

// Length of the in order prefix of messages already present as the leading live rows, stamping their row ids
func stampKeptLivePrefix(live []map[string]any, messages []map[string]any) (int, error) {
	kept := 0
	for i := 0; i < len(live) && i < len(messages); i++ {
		row, msg := live[i], messages[i]
		role := any("unknown")
		if r, ok := msg["role"]; ok {
			role = r
		}
		if !pyrepr.Equal(row["role"], role) || !pyrepr.Equal(row["tool_call_id"], msg["tool_call_id"]) {
			break
		}
		mine, err := rowIdentity(role, msg["content"], msg["tool_call_id"], parseToolCalls(msg["tool_calls"]))
		if err != nil {
			return kept, err
		}
		theirs, err := rowIdentity(row["role"], decodeMessageContent(row["content"]), row["tool_call_id"], parseToolCalls(row["tool_calls"]))
		if err != nil {
			return kept, err
		}
		if !pyrepr.Equal(mine, theirs) {
			break
		}
		msg["_row_id"] = row["id"]
		if pyTruthy(row["message_uid"]) {
			msg[MessageUID] = row["message_uid"]
		}
		kept++
	}
	return kept, nil
}

// Whether the session has any soft archived rows
func (d *SessionDB) HasArchivedMessages(ctx context.Context, sessionID string) (bool, error) {
	row, err := d.readOne(ctx, "SELECT 1 FROM messages WHERE session_id = ? AND active = 0 LIMIT 1", sessionID)
	return row != nil, err
}

// Highest active row id, zero when none, captured when compression starts
func (d *SessionDB) GetActiveMessageWatermark(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	row, err := d.readOne(ctx, "SELECT COALESCE(MAX(id), 0) AS n FROM messages WHERE session_id = ? AND active = 1", sessionID)
	if err != nil || row == nil {
		return 0, err
	}
	v, _ := row.Get("n")
	n, _ := pyIntID(v)
	return n, nil
}

// Rewrites the content of one active user row, replacing display metadata and clearing api_content when given
func (d *SessionDB) SetUserMessageContent(ctx context.Context, sessionID string, rowID int64, content any, displayMetadata any) (int64, error) {
	if sessionID == "" || rowID <= 0 {
		return 0, nil
	}
	if displayMetadata == nil {
		return d.writeRowcount(ctx, "UPDATE messages SET content = ? WHERE id = ? AND session_id = ? AND role = 'user' AND active = 1",
			[]any{encodeMessageContent(content), rowID, sessionID}, nil)
	}
	meta, err := encodeDisplayMetadata(displayMetadata)
	if err != nil {
		return 0, err
	}
	return d.writeRowcount(ctx, "UPDATE messages SET content = ?, api_content = NULL, display_metadata = ? WHERE id = ? AND session_id = ? AND role = 'user' AND active = 1",
		[]any{encodeMessageContent(content), meta, rowID, sessionID}, nil)
}

// Deactivates one known row idempotently, returning the affected row count
func (d *SessionDB) DeactivateMessage(ctx context.Context, sessionID string, rowID int64) (int64, error) {
	if sessionID == "" || rowID <= 0 {
		return 0, nil
	}
	return d.writeRowcount(ctx, "UPDATE messages SET active = 0 WHERE id = ? AND session_id = ?", []any{rowID, sessionID}, nil)
}

// Active row still carrying a row's message, itself while active or its unique compaction clone
func (d *SessionDB) ResolveActiveRowID(ctx context.Context, sessionID string, rowID int64) (int64, bool, error) {
	if sessionID == "" || rowID <= 0 {
		return 0, false, nil
	}
	origin, err := d.readOne(ctx, "SELECT active FROM messages WHERE id = ? AND session_id = ?", rowID, sessionID)
	if err != nil || origin == nil {
		return 0, false, err
	}
	if active, _ := origin.Get("active"); pyTruthy(active) {
		return rowID, true, nil
	}
	clones, err := d.readAll(ctx, "SELECT c.id AS id FROM messages c JOIN messages o ON o.id = ? WHERE c.session_id = ? AND c.active = 1 AND c.id > o.id AND c.role = o.role "+
		"AND c.content IS o.content AND c.timestamp = o.timestamp", rowID, sessionID)
	if err != nil || len(clones) != 1 {
		return 0, false, err
	}
	v, _ := clones[0].Get("id")
	id, _ := pyIntID(v)
	return id, true, nil
}

// Deactivates every live row of one display kind, returning the affected row count
func (d *SessionDB) DeactivateMessagesByDisplayKind(ctx context.Context, sessionID, displayKind string) (int64, error) {
	if sessionID == "" || displayKind == "" {
		return 0, nil
	}
	return d.writeRowcount(ctx, "UPDATE messages SET active = 0 WHERE session_id = ? AND display_kind = ? AND active = 1",
		[]any{sessionID, scrubSurrogates(displayKind)}, nil)
}

// Rows a display read of the segment paints, one per display order group
func (d *SessionDB) DisplayMessageCount(ctx context.Context, sessionID string) (int64, error) {
	row, err := d.readOne(ctx, "SELECT COUNT(*) AS n FROM (SELECT DISTINCT display_order FROM messages WHERE session_id = ?"+
		displayActiveClause+DisplayVisibleSQL+notALaterToolCopySQL("messages")+")", sessionID)
	if err != nil || row == nil {
		return 0, err
	}
	v, _ := row.Get("n")
	n, _ := pyIntID(v)
	return n, nil
}

// Tool results containing a pull request path, oldest first
func (d *SessionDB) FindPRURLMessages(ctx context.Context, sessionIDs []string) ([]map[string]any, error) {
	var ids []string
	for _, s := range sessionIDs {
		if s != "" {
			ids = append(ids, s)
		}
	}
	out := []map[string]any{}
	for _, chunk := range idChunks(ids, sqlInChunk) {
		args := make([]any, len(chunk))
		for i, s := range chunk {
			args[i] = s
		}
		rows, err := d.readAll(ctx, fmt.Sprintf("SELECT session_id, content FROM messages WHERE session_id IN (%s) AND role = 'tool' AND content LIKE '%%/pull/%%' ORDER BY id ASC",
			placeholders(len(chunk))), args...)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			sid, _ := r.Get("session_id")
			content, _ := r.Get("content")
			out = append(out, map[string]any{"session_id": sid, "content": content})
		}
	}
	return out, nil
}
