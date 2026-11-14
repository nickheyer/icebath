package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Error of a delegation delivery whose metadata names no stable unit id
var ErrDelegationIDRequired = errors.New("Delegation delivery requires a stable delegation_id")

// One reaction persisted on a message row
type Reaction struct {
	Emoji  string
	Author string
	// Epoch seconds the reaction was set
	At float64
	// Already announced to the model on a later user turn
	Seen bool
}

// One reaction not yet surfaced to the model
type PendingReaction struct {
	RowID int64
	Role  string
	Emoji string
	// Plain text of the reacted message, empty for structured content
	Text string
}

// Reaction entries of display metadata that are objects, with the members of each
func reactionEntries(meta []configMember) [][]configMember {
	raw, _ := memberValue(meta, ReactionsMetadataKey)
	elements, ok := arrayElements(raw)
	if !ok {
		return nil
	}
	var out [][]configMember
	for _, e := range elements {
		if members, ok := objectMembers(e); ok {
			out = append(out, members)
		}
	}
	return out
}

// Typed view of one stored reaction object
func reactionOf(members []configMember) Reaction {
	var r Reaction
	if v, ok := memberValue(members, "emoji"); ok {
		r.Emoji = jsonString(v)
	}
	if v, ok := memberValue(members, "author"); ok {
		r.Author = jsonString(v)
	}
	if v, ok := memberValue(members, "at"); ok {
		_ = json.Unmarshal(v, &r.At)
	}
	if v, ok := memberValue(members, "seen"); ok {
		r.Seen = jsonTruthy(v)
	}
	return r
}

// Reports whether a stored reaction object names the author
func reactionAuthorIs(members []configMember, author string) bool {
	v, ok := memberValue(members, "author")
	return ok && len(v) > 0 && v[0] == '"' && jsonString(v) == author
}

// Query and arguments addressing a row a display resume of the session can see
func (s *Store) reactionRowQuery(ctx context.Context, sessionID string, rowID int64) (string, []any, error) {
	lineage, err := s.ResumeLineageIDs(ctx, sessionID)
	if err != nil {
		return "", nil, err
	}
	return "SELECT display_metadata FROM messages WHERE id = ? AND session_id IN (" + bindMarks(len(lineage)) + ")" + displayActiveClause,
		append([]any{rowID}, stringArgs(lineage)...), nil
}

// Sets or clears the author's reaction on a visible row, the same emoji again clearing it, nil for a row outside the session's lineage
func (s *Store) SetMessageReaction(ctx context.Context, sessionID string, rowID int64, emoji, author string) ([]Reaction, bool, error) {
	if sessionID == "" {
		return nil, false, nil
	}
	if author == "" {
		author = "user"
	}
	query, args, err := s.reactionRowQuery(ctx, sessionID, rowID)
	if err != nil {
		return nil, false, err
	}
	var out []Reaction
	found := false
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		out, found = nil, false
		var cell any
		err := c.QueryRowContext(ctx, query, args...).Scan(&cell)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		meta, _ := objectMembers(decodeDisplayMetadata(cell))
		var kept [][]configMember
		var previous []configMember
		for _, r := range reactionEntries(meta) {
			if reactionAuthorIs(r, author) {
				if previous == nil {
					previous = r
				}
				continue
			}
			kept = append(kept, r)
		}
		if emoji != "" && (previous == nil || reactionOf(previous).Emoji != emoji) {
			emojiJSON, err := asciiJSONString(replaceSurrogates(emoji))
			if err != nil {
				return err
			}
			authorJSON, err := asciiJSONString(author)
			if err != nil {
				return err
			}
			kept = append(kept, []configMember{{"emoji", json.RawMessage(emojiJSON)}, {"author", json.RawMessage(authorJSON)},
				{"at", json.RawMessage(shortestFloatText(nowEpoch()))}})
		}
		out = make([]Reaction, len(kept))
		elements := make([]json.RawMessage, len(kept))
		for i, r := range kept {
			out[i] = reactionOf(r)
			text, err := encodeConfigMembers(r)
			if err != nil {
				return err
			}
			elements[i] = json.RawMessage(text)
		}
		if len(kept) > 0 {
			list, err := encodeArray(elements)
			if err != nil {
				return err
			}
			meta = setConfigMember(meta, ReactionsMetadataKey, json.RawMessage(list))
		} else {
			meta = dropMember(meta, ReactionsMetadataKey)
		}
		var value any
		if len(meta) > 0 {
			text, err := encodeConfigMembers(meta)
			if err != nil {
				return err
			}
			value = text
		}
		_, err = c.ExecContext(ctx, setDisplayMetaSQL, value, rowID)
		return err
	})
	return out, found, err
}

// Reactions persisted on one visible message row, none for a row outside the session's lineage
func (s *Store) GetMessageReactions(ctx context.Context, sessionID string, rowID int64) ([]Reaction, error) {
	if sessionID == "" {
		return nil, nil
	}
	query, args, err := s.reactionRowQuery(ctx, sessionID, rowID)
	if err != nil {
		return nil, err
	}
	var out []Reaction
	err = s.Read(ctx, func(c Conn) error {
		out = nil
		var cell any
		err := c.QueryRowContext(ctx, query, args...).Scan(&cell)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		meta, _ := objectMembers(decodeDisplayMetadata(cell))
		for _, r := range reactionEntries(meta) {
			out = append(out, reactionOf(r))
		}
		return nil
	})
	return out, err
}

// Returns the author's reactions not yet surfaced and marks them seen, compaction archived history included
func (s *Store) TakeUnseenReactions(ctx context.Context, sessionID, author string) ([]PendingReaction, error) {
	if sessionID == "" {
		return nil, nil
	}
	if author == "" {
		author = "user"
	}
	lineage, err := s.ResumeLineageIDs(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var pending []PendingReaction
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		pending = nil
		rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT id, role, content, display_metadata FROM messages "+
			"WHERE session_id IN ("+bindMarks(len(lineage))+")"+displayActiveClause+" "+
			"AND "+JSONExtractSQL("display_metadata", "$."+ReactionsMetadataKey)+" IS NOT NULL "+
			"ORDER BY id", stringArgs(lineage)...))
		if err != nil {
			return err
		}
		for _, r := range rows {
			meta, ok := objectMembers(decodeDisplayMetadata(r.displayMetadata))
			if !ok {
				continue
			}
			raw, _ := memberValue(meta, ReactionsMetadataKey)
			elements, ok := arrayElements(raw)
			if !ok {
				continue
			}
			changed := false
			for i, e := range elements {
				members, ok := objectMembers(e)
				if !ok || !reactionAuthorIs(members, author) {
					continue
				}
				if seen, _ := memberValue(members, "seen"); jsonTruthy(seen) {
					continue
				}
				text, err := encodeConfigMembers(setConfigMember(members, "seen", json.RawMessage("true")))
				if err != nil {
					return err
				}
				elements[i] = json.RawMessage(text)
				changed = true
				content, _ := decodeContent(r.content).Text()
				pending = append(pending, PendingReaction{RowID: r.id, Role: r.roleText(), Emoji: reactionOf(members).Emoji, Text: content})
			}
			if !changed {
				continue
			}
			list, err := encodeArray(elements)
			if err != nil {
				return err
			}
			text, err := encodeConfigMembers(setConfigMember(meta, ReactionsMetadataKey, json.RawMessage(list)))
			if err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, setDisplayMetaSQL, text, r.id); err != nil {
				return err
			}
		}
		return nil
	})
	return pending, err
}

// Stamps presentation metadata on the newest active row holding this role and content, true when one was found
func (s *Store) SetLatestMatchingMessageDisplayKind(ctx context.Context, sessionID, role, content, displayKind string, displayMetadata json.RawMessage) (bool, error) {
	if sessionID == "" || content == "" || displayKind == "" {
		return false, nil
	}
	encoded, err := TextContent(content).encode()
	if err != nil {
		return false, err
	}
	meta, err := encodeDisplayMetadata(displayMetadata)
	if err != nil {
		return false, err
	}
	found := false
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		found = false
		var id int64
		err := c.QueryRowContext(ctx, "SELECT id FROM messages WHERE session_id = ? AND role = ? "+
			"AND content = ? AND active = 1 ORDER BY id DESC LIMIT 1", sessionID, role, encoded).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = c.ExecContext(ctx, "UPDATE messages SET display_kind = ?, display_metadata = ? WHERE id = ?", replaceSurrogates(displayKind), meta, id)
		found = err == nil
		return err
	})
	return found, err
}

// Rewrites the content of one known active user row, display metadata when given replacing the row's and clearing its API sidecar
func (s *Store) SetUserMessageContent(ctx context.Context, sessionID string, rowID int64, content Content, displayMetadata json.RawMessage) (int64, error) {
	if sessionID == "" || rowID <= 0 {
		return 0, nil
	}
	encoded, err := content.encode()
	if err != nil {
		return 0, err
	}
	if displayMetadata == nil {
		return s.WriteRowcount(ctx, WritePatience, "UPDATE messages SET content = ? WHERE id = ? AND session_id = ? AND role = 'user' AND active = 1",
			encoded, rowID, sessionID)
	}
	meta, err := encodeDisplayMetadata(displayMetadata)
	if err != nil {
		return 0, err
	}
	return s.WriteRowcount(ctx, WritePatience, "UPDATE messages SET content = ?, api_content = NULL, display_metadata = ? "+
		"WHERE id = ? AND session_id = ? AND role = 'user' AND active = 1", encoded, meta, rowID, sessionID)
}

// Deactivates one known row, the durable row kept, returning rows changed
func (s *Store) DeactivateMessage(ctx context.Context, sessionID string, rowID int64) (int64, error) {
	if sessionID == "" || rowID <= 0 {
		return 0, nil
	}
	return s.WriteRowcount(ctx, WritePatience, "UPDATE messages SET active = 0 WHERE id = ? AND session_id = ?", rowID, sessionID)
}

// Active row that still carries a row's message, the row itself or the one clone a compaction made of it, false otherwise
func (s *Store) ResolveActiveRowID(ctx context.Context, sessionID string, rowID int64) (int64, bool, error) {
	if sessionID == "" || rowID <= 0 {
		return 0, false, nil
	}
	active, found, err := s.readInt(ctx, "SELECT active FROM messages WHERE id = ? AND session_id = ?", rowID, sessionID)
	if err != nil || !found {
		return 0, false, err
	}
	if active != 0 {
		return rowID, true, nil
	}
	var clones []int64
	err = s.Read(ctx, func(c Conn) error {
		var err error
		clones, err = queryInt64s(ctx, c, "SELECT c.id FROM messages c JOIN messages o ON o.id = ? "+
			"WHERE c.session_id = ? AND c.active = 1 AND c.id > o.id AND c.role = o.role "+
			"AND c.content IS o.content AND c.timestamp = o.timestamp", rowID, sessionID)
		return err
	})
	if err != nil || len(clones) != 1 {
		return 0, false, err
	}
	return clones[0], true, nil
}

// Deactivates every live row of one display kind, returning rows changed
func (s *Store) DeactivateMessagesByDisplayKind(ctx context.Context, sessionID, displayKind string) (int64, error) {
	if sessionID == "" || displayKind == "" {
		return 0, nil
	}
	return s.WriteRowcount(ctx, WritePatience, "UPDATE messages SET active = 0 WHERE session_id = ? AND display_kind = ? AND active = 1",
		sessionID, replaceSurrogates(displayKind))
}

// Backfills the API sidecar onto the newest active user row while its content still matches, returning rows changed
func (s *Store) SetLatestUserAPIContent(ctx context.Context, sessionID string, content Content, apiContent string) (int64, error) {
	encoded, err := content.encode()
	if err != nil {
		return 0, err
	}
	return s.WriteRowcount(ctx, WritePatience, "UPDATE messages SET api_content = ? WHERE id = (SELECT id FROM messages "+
		"WHERE session_id = ? AND role = 'user' AND active = 1 ORDER BY id DESC LIMIT 1"+
		") AND content IS ?", replaceSurrogates(apiContent), sessionID, encoded)
}

// Backfills the API sidecar onto one known active user row while its content still matches, returning rows changed
func (s *Store) SetMessageAPIContent(ctx context.Context, sessionID string, rowID int64, content Content, apiContent string) (int64, error) {
	if sessionID == "" || rowID <= 0 {
		return 0, nil
	}
	encoded, err := content.encode()
	if err != nil {
		return 0, err
	}
	return s.WriteRowcount(ctx, WritePatience, "UPDATE messages SET api_content = ? WHERE id = ? AND session_id = ? "+
		"AND role = 'user' AND active = 1 AND content IS ?", replaceSurrogates(apiContent), rowID, sessionID, encoded)
}

// Rows of a session and of the parents it was compressed from, a match condition on m following
const lineageMatchSQL = `WITH RECURSIVE lineage(id) AS (SELECT ? UNION
    SELECT s.parent_session_id FROM sessions s JOIN lineage l ON s.id = l.id
    JOIN sessions p ON p.id = s.parent_session_id WHERE p.end_reason = 'compression'
) SELECT m.id FROM messages m JOIN lineage l ON m.session_id = l.id WHERE `

// Match condition of a delegation delivery, its unit id and delivery notice
const delegationDeliveryMatch = "m.display_kind IN ('async_delegation_complete', 'hidden') AND json_extract(m.display_metadata, " +
	"'$.delegation_id') = ? AND coalesce(json_extract(m.display_metadata, '$.delivery_notice'), '') = ?"

// Records a detached delegation result once, keyed by the unit id its metadata carries
func (s *Store) AppendDelegationDelivery(ctx context.Context, sessionID, content string, metadata json.RawMessage) (int64, error) {
	members, _ := objectMembers(metadata)
	rawID, _ := memberValue(members, "delegation_id")
	if !jsonTruthy(rawID) {
		return 0, ErrDelegationIDRequired
	}
	delegationID, err := jsonBindValue(rawID)
	if err != nil {
		return 0, err
	}
	kind := "async_delegation_complete"
	if suppressed, _ := memberValue(members, "presentation_suppressed"); jsonTruthy(suppressed) {
		kind = "hidden"
	}
	notice := any("")
	if raw, ok := memberValue(members, "delivery_notice"); ok {
		if notice, err = jsonBindValue(raw); err != nil {
			return 0, err
		}
	}
	return s.appendDetachedResult(ctx, sessionID, content, kind, metadata, delegationDeliveryMatch, delegationID, notice)
}

// Records an unviewed completion's note once, keyed by its admission
func (s *Store) AppendCompletionNote(ctx context.Context, sessionID, admissionID, content, displayKind string, metadata json.RawMessage) (int64, error) {
	members, _ := objectMembers(metadata)
	admission, err := json.Marshal(admissionID)
	if err != nil {
		return 0, err
	}
	text, err := encodeConfigMembers(setConfigMember(append([]configMember{}, members...), "completion_admission", admission))
	if err != nil {
		return 0, err
	}
	return s.appendDetachedResult(ctx, sessionID, content, displayKind, json.RawMessage(text),
		"json_extract(m.display_metadata, '$.completion_admission') = ?", admissionID)
}

// Records a result that arrived between turns as a user row once across the compression lineage, a live turn refusing it
func (s *Store) appendDetachedResult(ctx context.Context, sessionID, content, displayKind string, metadata json.RawMessage, matchSQL string, params ...any) (int64, error) {
	msg := Message{Role: "user", Content: TextContent(content), DisplayMetadata: metadata}
	if displayKind != "" {
		msg.DisplayKind = sql.Null[string]{V: displayKind, Valid: true}
	}
	if err := stampMessageUID(&msg); err != nil {
		return 0, err
	}
	row, err := s.messageRowParams(sessionID, "user", &msg, nil, nowEpoch(), true)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.Write(ctx, TranscriptWritePatience, func(c Conn) error {
		err := c.QueryRowContext(ctx, lineageMatchSQL+matchSQL+" LIMIT 1", append([]any{sessionID}, params...)...).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := s.checkTranscriptWriteGuards(ctx, c, sessionID, writeGuards{rejectActiveTurnLease: true}); err != nil {
			return err
		}
		res, err := c.ExecContext(ctx, insertMessageSQL, row.insertArgs()...)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return bumpSessionCounters(ctx, c, sessionID, 1, 0, true)
	})
	return id, err
}

// Deletes every message of a session and resets its counters
func (s *Store) ClearMessages(ctx context.Context, sessionID string) error {
	return s.Write(ctx, WritePatience, func(c Conn) error {
		if _, err := c.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx, resetCountersSQL, sessionID)
		return err
	})
}

// Outcome of a stale marker purge
type MarkerPurge struct {
	DryRun       bool
	RowsAffected int
	RowIDs       []int64
	// Snapshot taken before the write, empty when none was made
	BackupPath string
}

// Ids of assistant tool call rows whose content is a bare marker
func staleMarkerRowIDs(ctx context.Context, c Conn) ([]int64, error) {
	rows, err := c.QueryContext(ctx, "SELECT id, content FROM messages "+
		"WHERE role = 'assistant' AND tool_calls IS NOT NULL AND tool_calls != ''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		var content any
		if err := rows.Scan(&id, &content); err != nil {
			return nil, err
		}
		if text, ok := content.(string); ok && staleToolCallMarker.MatchString(strings.TrimFunc(text, previewSpace)) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// Permanently clears bare tool call marker content left by earlier builds, snapshotting the store first when asked
func (s *Store) PurgeStaleToolCallMarkers(ctx context.Context, dryRun, backup bool) (MarkerPurge, error) {
	out := MarkerPurge{DryRun: dryRun}
	err := s.Read(ctx, func(c Conn) error {
		var err error
		out.RowIDs, err = staleMarkerRowIDs(ctx, c)
		return err
	})
	out.RowsAffected = len(out.RowIDs)
	if err != nil || dryRun || len(out.RowIDs) == 0 {
		return out, err
	}
	if backup {
		path := s.path + ".pre-clean-markers-backup-" + time.Now().Format("20060102_150405")
		if err := s.onWriter(ctx, func(c Conn) error {
			_, err := c.ExecContext(ctx, "VACUUM INTO ?", path)
			return err
		}); err != nil {
			return out, presentable(err)
		}
		out.BackupPath = path
		slog.Info(fmt.Sprintf("Backed up state.db to %s before clean-markers write", path))
	}
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		ids, err := staleMarkerRowIDs(ctx, c)
		if err != nil {
			return err
		}
		out.RowIDs = ids
		if len(ids) == 0 {
			return nil
		}
		_, err = c.ExecContext(ctx, "UPDATE messages SET content = '' WHERE id IN ("+bindMarks(len(ids))+")", int64Args(ids)...)
		return err
	})
	if err != nil {
		return out, err
	}
	out.RowsAffected = len(out.RowIDs)
	if len(out.RowIDs) > 0 {
		slog.Info(fmt.Sprintf("Permanently cleared %d stale tool-call marker row(s) in state.db (#78148)", len(out.RowIDs)))
	}
	return out, nil
}

// How a transcript replacement treats the rows it replaces
type ReplaceOptions struct {
	// Spares soft archived rows, deleting only live ones
	ActiveOnly bool
	// Soft archives the divergent live suffix instead of deleting, keeping the common prefix in place
	ArchiveDropped bool
	// Refuses while another holder owns a live turn lease or compression lock
	RejectActiveTurnLease bool
	// Active row ids the replacement was computed from, nil when unpinned
	ExpectedActiveIDs []int64
}

// Atomically replaces a session's messages
func (s *Store) ReplaceMessages(ctx context.Context, sessionID string, messages []*Message, o ReplaceOptions) error {
	return s.transcriptWrite(ctx, WritePatience, messages, func(c Conn) error {
		return s.ReplaceMessagesIn(ctx, c, sessionID, messages, o)
	})
}

// Identity the kept prefix match compares, role, loaded view content, tool call id and tool calls
func rowIdentity(role string, content Content, toolCallID any, toolCalls json.RawMessage) (string, error) {
	encoded, err := loadedViewContent(role, content).encode()
	if err != nil {
		return "", err
	}
	var calls any
	if jsonTruthy(toolCalls) {
		text, err := spacedJSON(toolCalls)
		if err != nil {
			return "", err
		}
		calls = text
	}
	return contentKeyText(role, encoded, nil, toolCallID, calls, nil), nil
}

// Identity of a stored row as the kept prefix match compares it
func storedRowIdentity(r *msgRow) (string, error) {
	var calls json.RawMessage
	if text, ok := r.toolCalls.(string); ok {
		calls = parseStoredJSON(text)
	}
	return rowIdentity(r.roleText(), decodeContent(r.content), r.toolCallID, calls)
}

// Identity of a live message as the kept prefix match compares it
func liveRowIdentity(m *Message) (string, error) {
	return rowIdentity(m.role(), m.Content, nullText(m.ToolCallID), parseToolCalls(m.ToolCalls))
}

// Length of the leading run of messages already present as the leading live rows, stamping each match with its row
func stampKeptLivePrefix(live []*msgRow, messages []*Message) (int, error) {
	kept := 0
	for i := 0; i < len(live) && i < len(messages); i++ {
		row, m := live[i], messages[i]
		if row.roleText() != m.role() || !cellsEqual(row.toolCallID, nullText(m.ToolCallID)) {
			break
		}
		want, err := liveRowIdentity(m)
		if err != nil {
			return 0, err
		}
		have, err := storedRowIdentity(row)
		if err != nil {
			return 0, err
		}
		if want != have {
			break
		}
		m.RowID = row.id
		if uid := textCell(row.messageUID); uid != "" {
			m.UID = uid
		}
		kept++
	}
	return kept, nil
}

// Replacement inside a transaction the caller owns
func (s *Store) ReplaceMessagesIn(ctx context.Context, c Conn, sessionID string, messages []*Message, o ReplaceOptions) error {
	if o.RejectActiveTurnLease {
		if err := s.checkTranscriptWriteGuards(ctx, c, sessionID, writeGuards{rejectActiveTurnLease: true, rejectActiveCompressionLock: true}); err != nil {
			return err
		}
	} else if closed, err := endedByCompression(ctx, c, sessionID); err != nil {
		return err
	} else if closed {
		return &CompressionClosedError{SessionID: sessionID}
	}
	if o.ExpectedActiveIDs != nil {
		active, err := queryInt64s(ctx, c, activeIDsSQL, sessionID)
		if err != nil {
			return err
		}
		if !equalInt64s(active, o.ExpectedActiveIDs) {
			return ErrTranscriptChanged
		}
	}
	kept, keptToolCalls := 0, 0
	if o.ArchiveDropped {
		live, err := queryMsgRows(c.QueryContext(ctx, liveIdentitySQL, sessionID, len(messages)+1))
		if err != nil {
			return err
		}
		if kept, err = stampKeptLivePrefix(live, messages); err != nil {
			return err
		}
		for _, row := range live[:kept] {
			keptToolCalls += toolCallsLen(row.toolCalls, 1)
		}
		if kept < len(live) {
			if _, err := c.ExecContext(ctx, "UPDATE messages SET active = 0 WHERE session_id = ? AND active = 1 AND id >= ?", sessionID, live[kept].id); err != nil {
				return err
			}
		}
	} else {
		query := "DELETE FROM messages WHERE session_id = ?"
		if o.ActiveOnly {
			query += " AND active = 1"
		}
		if _, err := c.ExecContext(ctx, query, sessionID); err != nil {
			return err
		}
	}
	inserted, insertedToolCalls, err := s.insertMessageRows(ctx, c, sessionID, messages[kept:], true)
	if err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, setCountersSQL+" WHERE id = ?", kept+inserted, keptToolCalls+insertedToolCalls, sessionID)
	return err
}

// Reports whether two id lists hold the same ids in the same order
func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
