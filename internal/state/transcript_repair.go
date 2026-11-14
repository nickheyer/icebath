package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Whitespace class the context fence patterns accept between tag parts
const fenceSpace = `[\s\x0b\x1c-\x1f\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

// Recalled memory fences, injected context blocks and their system notes, stripped from text that leaves the agent
var contextFencePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<` + fenceSpace + `*memory-context` + fenceSpace + `*>.*?</` + fenceSpace + `*memory-context` + fenceSpace + `*>`),
	regexp.MustCompile(`(?i)\[System note:` + fenceSpace + `*The following is recalled memory context,` + fenceSpace +
		`*NOT new user input\.` + fenceSpace + `*Treat as (?:informational background data|authoritative reference data[^\]]*)\.\]` + fenceSpace + `*`),
	regexp.MustCompile(`(?i)</?` + fenceSpace + `*memory-context` + fenceSpace + `*>`),
}

// Text with memory fence tags, injected context blocks and system notes removed
func SanitizeContext(text string) string {
	for _, p := range contextFencePatterns {
		text = p.ReplaceAllString(text, "")
	}
	return text
}

// Content as a conversation read hands it to callers, user and assistant text sanitized and trimmed
func loadedViewContent(role string, c Content) Content {
	if role != "user" && role != "assistant" {
		return c
	}
	text, ok := c.Text()
	if !ok {
		return c
	}
	return TextContent(strings.TrimFunc(SanitizeContext(text), previewSpace))
}

// Reports decoded content that is absent, whitespace only, or a list without visible text parts
func isContentBlank(c Content) bool {
	if c.IsNull() {
		return true
	}
	if text, ok := c.Text(); ok {
		return strings.TrimFunc(text, previewSpace) == ""
	}
	parts, ok := contentParts(c)
	if !ok {
		return false
	}
	var b strings.Builder
	for _, p := range parts {
		if p.partType() == "text" {
			text, _ := p.partText()
			b.WriteString(text)
		}
	}
	return strings.TrimFunc(b.String(), previewSpace) == ""
}

// Newest active physical row of one durable logical message, nil when it has none
func activeLogicalMessageRow(ctx context.Context, c Conn, sessionID, role, uid string) (*msgRow, error) {
	if uid == "" {
		return nil, nil
	}
	return queryMsgRow(c.QueryContext(ctx, "SELECT * FROM messages WHERE session_id = ? AND active = 1 AND role = ? AND message_uid = ? "+
		"ORDER BY id DESC LIMIT 1", sessionID, role, uid))
}

// Active durable rows by identity among the restored messages that carry neither a row id nor a version digest
func activeUIDDedupe(ctx context.Context, c Conn, sessionID string, messages []*Message) (map[string][]*msgRow, error) {
	var candidates []string
	for _, m := range messages {
		if m != nil && m.RestoredFromStore && m.RowID == 0 && m.Snapshot == "" && m.UID != "" && !containsString(candidates, m.UID) {
			candidates = append(candidates, m.UID)
		}
	}
	sort.Strings(candidates)
	durable := map[string][]*msgRow{}
	for _, chunk := range chunks(candidates, sqlInChunk) {
		rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT * FROM messages WHERE session_id = ? AND active = 1 AND message_uid IN ("+
			bindMarks(len(chunk))+")", append([]any{sessionID}, stringArgs(chunk)...)...))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			uid := textCell(r.messageUID)
			durable[uid] = append(durable[uid], r)
		}
	}
	return durable, nil
}

// Reports whether a message serializes to the replayed columns of one of its identity's active rows
func (s *Store) sameAsStored(sessionID string, m *Message, rows []*msgRow) (bool, error) {
	for _, row := range rows {
		ts, _ := floatCell(row.timestamp)
		serialized, err := s.serializedMessageRow(sessionID, m, ts)
		if err != nil {
			return false, err
		}
		if !m.TokenCount.Valid || m.TokenCount.V == 0 {
			serialized.tokenCount = row.tokenCount
		}
		if !m.CompressedSummary {
			serialized.compressedSummary = row.compressedSummary
		}
		if len(m.ToolCallUIDs) == 0 {
			serialized.toolCallUIDs = row.toolCallUIDs
		}
		if len(m.AbsorbedUIDs) == 0 {
			serialized.absorbedMessageUIDs = row.absorbedMessageUIDs
		}
		if lens, err := loadedViewContent(row.roleText(), decodeContent(row.content)).encode(); err == nil && cellsEqual(serialized.content, lens) {
			serialized.content = row.content
		}
		same := true
		for _, col := range replayColumns {
			if !cellsEqual(row.col(col), serialized.col(col)) {
				same = false
				break
			}
		}
		if same {
			return true, nil
		}
	}
	return false, nil
}

// Same role active clone of an addressed row, or the addressed row itself when no single clone exists
func activeMessageRow(ctx context.Context, c Conn, sessionID string, rowID int64, role string) (*msgRow, error) {
	row, err := queryMsgRow(c.QueryContext(ctx, "SELECT * FROM messages WHERE id = ? AND session_id = ?", rowID, sessionID))
	if err != nil || row == nil || row.roleText() != role {
		return nil, err
	}
	if active, _ := cellInt(row.active); active == 1 {
		return row, nil
	}
	if row.displayIdentity == nil {
		return row, nil
	}
	predicates := make([]string, len(repairColumns))
	args := []any{sessionID, role, row.displayIdentity, row.timestamp, row.id}
	for i, col := range repairColumns {
		predicates[i] = col + " IS ?"
		args = append(args, row.col(col))
	}
	clones, err := queryMsgRows(c.QueryContext(ctx, "SELECT * FROM messages WHERE session_id = ? AND active = 1 AND role = ? "+
		"AND display_identity IS ? AND timestamp IS ? AND id != ? AND "+strings.Join(predicates, " AND ")+" ORDER BY id DESC LIMIT 2", args...))
	if err != nil {
		return nil, err
	}
	if len(clones) == 1 {
		return clones[0], nil
	}
	return row, nil
}

// Provider call ids the tool calls of a live message name
func namedCallIDs(toolCalls json.RawMessage) ([]string, bool) {
	elements, ok := arrayElements(toolCalls)
	if !ok {
		return nil, false
	}
	ids := make([]string, 0, len(elements))
	for _, raw := range elements {
		callID, id, _ := toolCallIDFields(raw)
		ids = append(ids, CoalesceToolCallID(callID, id))
	}
	return ids, true
}

// Keeps the stored identity of calls a message still names but lost, before a row addressed rewrite serializes it
func fillMissingToolCallUIDs(row *msgRow, m *Message) {
	if !cellTruthy(row.toolCallUIDs) {
		return
	}
	ids, ok := namedCallIDs(m.ToolCalls)
	if !ok {
		return
	}
	live := m.ToolCallUIDs
	if live == nil {
		live = ToolCallUIDs{}
	}
	m.ToolCallUIDs = FillMissingToolCallUIDs(DecodeToolCallUIDs(textCell(row.toolCallUIDs)), live, ids)
	if len(m.ToolCallUIDs) == 0 {
		m.ToolCallUIDs = nil
	}
}

// Stored identity of a row onto its live message, the stored value winning
func restoreRowIdentity(row *msgRow, m *Message) {
	if uid := textCell(row.messageUID); uid != "" {
		m.UID = uid
	}
	if cellTruthy(row.toolCallUIDs) {
		if stored := DecodeToolCallUIDs(textCell(row.toolCallUIDs)); len(stored) > 0 {
			live := m.ToolCallUIDs
			if live == nil {
				live = ToolCallUIDs{}
			}
			m.ToolCallUIDs = RestoreToolCallUIDs(live, stored)
		}
	}
	if uid := textCell(row.toolCallUID); uid != "" {
		m.ToolCallUID = uid
	}
}

// Stored identity columns of a row onto a restored message, empty columns adding nothing
func restoreIdentityColumns(row *msgRow, m *Message) {
	restoreRowIdentity(row, m)
	if cellTruthy(row.absorbedMessageUIDs) {
		if absorbed := DecodeUIDList(textCell(row.absorbedMessageUIDs)); len(absorbed) > 0 {
			m.AbsorbedUIDs = absorbed
		}
	}
}

// Nullable text of a cell, absent for NULL
func nullCell(v any) sql.Null[string] {
	if v == nil {
		return sql.Null[string]{}
	}
	text, _ := cellText(v)
	return sql.Null[string]{V: text, Valid: true}
}

// Stored JSON text of a cell as a value, nil with a warning for text that is not JSON
func jsonCell(v any, warning string) json.RawMessage {
	text, ok := v.(string)
	if !ok || !json.Valid([]byte(text)) {
		slog.Warn(warning)
		return nil
	}
	return json.RawMessage(text)
}

// One durable row decoded without replay dedupe, alternation repair or content stripping
func decodedRepairRow(row *msgRow) Message {
	m := Message{Role: row.roleText(), Content: decodeContent(row.content), ToolCallID: nullCell(row.toolCallID),
		ToolName: nullCell(row.toolName), EffectDisposition: nullCell(row.effectDisposition), FinishReason: nullCell(row.finishReason),
		PlatformMessageID: nullCell(row.platformMessageID), Observed: cellTruthy(row.observed),
		CompressedSummary: cellTruthy(row.compressedSummary), APIContent: nullCell(row.apiContent), DisplayKind: nullCell(row.displayKind)}
	if n, ok := cellInt(row.tokenCount); ok && row.tokenCount != nil {
		m.TokenCount = sql.Null[int64]{V: n, Valid: true}
	}
	if cellTruthy(row.toolCalls) {
		if m.ToolCalls = jsonCell(row.toolCalls, "Failed to deserialize repaired tool_calls, falling back to []"); m.ToolCalls == nil {
			m.ToolCalls = json.RawMessage("[]")
		}
	}
	restoreIdentityColumns(row, &m)
	if row.displayMetadata != nil {
		m.DisplayMetadata = decodeDisplayMetadata(row.displayMetadata)
	}
	if m.Role == "assistant" {
		m.Reasoning, m.ReasoningContent = nullCell(row.reasoning), nullCell(row.reasoningContent)
		for _, f := range []struct {
			dst  *json.RawMessage
			cell any
			name string
		}{{&m.ReasoningDetails, row.reasoningDetails, "reasoning_details"}, {&m.CodexReasoningItems, row.codexReasoningItems, "codex_reasoning_items"},
			{&m.CodexMessageItems, row.codexMessageItems, "codex_message_items"}} {
			if cellTruthy(f.cell) {
				*f.dst = jsonCell(f.cell, "Failed to deserialize repaired "+f.name+", falling back to None")
			}
		}
	}
	return m
}

// Rewrites the owned columns of one durable row whose digest matched the live message's last committed version
func rewriteRow(ctx context.Context, c Conn, sessionID string, target, serialized *msgRow) error {
	oldIdentity := target.displayIdentity
	var oldPeers []int64
	if oldIdentity != nil {
		var err error
		if oldPeers, err = queryInt64s(ctx, c, "SELECT id FROM messages WHERE session_id = ? AND id != ? "+
			"AND (active = 1 OR compacted = 1) AND display_identity IS ?", sessionID, target.id, oldIdentity); err != nil {
			return err
		}
	}
	assignments := make([]string, len(ownedColumns))
	args := make([]any, 0, len(ownedColumns)+2)
	for i, col := range ownedColumns {
		assignments[i] = col + " = ?"
		args = append(args, serialized.col(col))
	}
	if _, err := c.ExecContext(ctx, "UPDATE messages SET "+strings.Join(assignments, ", ")+" WHERE id = ? AND session_id = ?",
		append(args, target.id, sessionID)...); err != nil {
		return err
	}
	return restoreDisplayIndex(ctx, c, sessionID, target, serialized, oldIdentity, oldPeers)
}

// First column of every row of a query as integers
func queryInt64s(ctx context.Context, c Conn, query string, args ...any) ([]int64, error) {
	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v sql.NullInt64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v.Int64)
	}
	return out, rows.Err()
}

// Restores the display identities and orders the payload update trigger invalidated
func restoreDisplayIndex(ctx context.Context, c Conn, sessionID string, target, serialized *msgRow, oldIdentity any, oldPeers []int64) error {
	if len(oldPeers) > 0 {
		args := append([]any{oldIdentity, slices.Min(oldPeers), sessionID}, int64Args(oldPeers)...)
		if _, err := c.ExecContext(ctx, "UPDATE messages SET display_identity = ?, display_order = ? "+
			"WHERE session_id = ? AND id IN ("+bindMarks(len(oldPeers))+")", args...); err != nil {
			return err
		}
	}
	newIdentity := serialized.displayIdentity
	if !cellTruthy(target.active) && !cellTruthy(target.compacted) {
		_, err := c.ExecContext(ctx, "UPDATE messages SET display_identity = ?, display_order = ? WHERE id = ? AND session_id = ?",
			newIdentity, target.id, target.id, sessionID)
		return err
	}
	rows, err := c.QueryContext(ctx, "SELECT id, display_order FROM messages WHERE session_id = ? AND id != ? "+
		"AND (active = 1 OR compacted = 1) AND display_identity IS ?", sessionID, target.id, newIdentity)
	if err != nil {
		return err
	}
	var peers []int64
	newOrder := target.id
	for rows.Next() {
		var id int64
		var order sql.NullInt64
		if err := rows.Scan(&id, &order); err != nil {
			rows.Close()
			return err
		}
		peers = append(peers, id)
		slot := id
		if order.Valid {
			slot = order.Int64
		}
		newOrder = min(newOrder, slot)
	}
	if err := rowsDone(rows); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "UPDATE messages SET display_identity = ?, display_order = ? WHERE id = ? AND session_id = ?",
		newIdentity, newOrder, target.id, sessionID); err != nil {
		return err
	}
	for _, id := range peers {
		if _, err := c.ExecContext(ctx, "UPDATE messages SET display_order = ? WHERE id = ? AND session_id = ?", newOrder, id, sessionID); err != nil {
			return err
		}
	}
	return nil
}

// Reports an absent or null JSON value
func jsonAbsent(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null"
}

// Resolves row addressed rewrites without appending duplicates or replacing concurrent winners, returning the messages that need fresh inserts
func (s *Store) resolveAndRepairBatch(ctx context.Context, c Conn, sessionID string, messages []*Message) ([]*Message, error) {
	var fresh []*Message
	durable, err := activeUIDDedupe(ctx, c, sessionID, messages)
	if err != nil {
		return nil, err
	}
	for _, m := range messages {
		role, expected := m.role(), m.Snapshot
		var target *msgRow
		if m.RowID != 0 {
			target, err = activeMessageRow(ctx, c, sessionID, m.RowID, role)
		} else if expected != "" {
			target, err = activeLogicalMessageRow(ctx, c, sessionID, role, m.UID)
		}
		if err != nil {
			return nil, err
		}
		if target == nil {
			if stored := durable[m.UID]; m.UID != "" && len(stored) > 0 && !m.PersistAfterAdmissionInterrupt {
				same, err := s.sameAsStored(sessionID, m, stored)
				if err != nil {
					return nil, err
				}
				if same {
					m.Persisted = true
					continue
				}
			}
			fresh = append(fresh, m)
			continue
		}
		m.RowID = target.id
		fillMissingToolCallUIDs(target, m)
		var canonical *CanonicalRow
		adopt, wrote := false, false
		switch {
		case expected != "":
			adopt = rowSnapshot(target) != expected
			if adopt {
				break
			}
			ts, _ := floatCell(target.timestamp)
			serialized, err := s.serializedMessageRow(sessionID, m, ts)
			if err != nil {
				return nil, err
			}
			if serialized.tokenCount == nil {
				serialized.tokenCount = target.tokenCount
			}
			for _, col := range ownedColumns {
				if !cellsEqual(target.col(col), serialized.col(col)) {
					if err := rewriteRow(ctx, c, sessionID, target, serialized); err != nil {
						return nil, err
					}
					wrote = true
					break
				}
			}
			lacksKind, lacksMeta := !m.DisplayKind.Valid, jsonAbsent(m.DisplayMetadata)
			if (lacksKind && target.displayKind != nil) || (lacksMeta && target.displayMetadata != nil) {
				decoded := decodedRepairRow(target)
				canonical = &CanonicalRow{MetadataOnly: true}
				if lacksKind {
					canonical.Message.DisplayKind = decoded.DisplayKind
				}
				if lacksMeta {
					canonical.Message.DisplayMetadata = decoded.DisplayMetadata
				}
			}
		case role == "assistant" && isContentBlank(decodeContent(target.content)):
			content, err := m.Content.encode()
			if err != nil {
				return nil, err
			}
			res, err := c.ExecContext(ctx, "UPDATE messages SET content = ? WHERE id = ? AND session_id = ? AND content IS ?",
				content, target.id, sessionID, target.content)
			if err != nil {
				return nil, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return nil, err
			}
			wrote = n > 0
		case role == "assistant":
			canonical = &CanonicalRow{ContentOnly: true, Message: Message{Content: decodeContent(target.content)}}
		}
		final := target
		if wrote {
			if final, err = queryMsgRow(c.QueryContext(ctx, "SELECT * FROM messages WHERE id = ? AND session_id = ?", target.id, sessionID)); err != nil {
				return nil, err
			}
			if final == nil {
				return nil, ErrSessionRowGone
			}
		}
		if ts, ok := floatCell(final.timestamp); ok {
			m.Timestamp = sql.Null[float64]{V: ts, Valid: true}
		}
		restoreRowIdentity(final, m)
		m.Snapshot = rowSnapshot(final)
		if adopt {
			canonical = &CanonicalRow{Message: decodedRepairRow(final)}
		}
		m.Canonical = canonical
	}
	return fresh, nil
}

// Stamps the persistence marker and the committed row state of each flushed row onto the live message it was written from
func SyncFlushedMessages(written, rows []*Message) {
	for i := 0; i < len(written) && i < len(rows); i++ {
		w, r := written[i], rows[i]
		w.Persisted = true
		if r.RowID != 0 {
			w.RowID = r.RowID
		}
		if r.UID != "" {
			w.UID = r.UID
		}
		if len(r.AbsorbedUIDs) > 0 {
			w.AbsorbedUIDs = slices.Clone(r.AbsorbedUIDs)
		}
		if len(r.ToolCallUIDs) > 0 {
			copied := ToolCallUIDs{}
			for k, v := range r.ToolCallUIDs {
				copied[k] = v
			}
			w.ToolCallUIDs = copied
		}
		if r.ToolCallUID != "" {
			w.ToolCallUID = r.ToolCallUID
		}
		if r.Timestamp.Valid {
			w.Timestamp = r.Timestamp
		}
		if r.Snapshot != "" {
			w.Snapshot = r.Snapshot
		}
		canonical := r.Canonical
		if canonical == nil {
			continue
		}
		cm := canonical.Message
		switch {
		case canonical.MetadataOnly:
			if cm.DisplayKind.Valid && !w.DisplayKind.Valid {
				w.DisplayKind = cm.DisplayKind
			}
			if !jsonAbsent(cm.DisplayMetadata) && jsonAbsent(w.DisplayMetadata) {
				w.DisplayMetadata = cm.DisplayMetadata
			}
		case canonical.ContentOnly:
			w.Content = cm.Content
		default:
			w.Role = cm.Role
			if !cm.Content.IsNull() {
				w.Content = cm.Content
			}
			w.ToolCallID, w.ToolCalls, w.ToolName, w.EffectDisposition = cm.ToolCallID, cm.ToolCalls, cm.ToolName, cm.EffectDisposition
			w.TokenCount, w.FinishReason, w.Reasoning, w.ReasoningContent = cm.TokenCount, cm.FinishReason, cm.Reasoning, cm.ReasoningContent
			w.ReasoningDetails, w.CodexReasoningItems, w.CodexMessageItems = cm.ReasoningDetails, cm.CodexReasoningItems, cm.CodexMessageItems
			w.PlatformMessageID, w.Observed, w.CompressedSummary = cm.PlatformMessageID, cm.Observed, cm.CompressedSummary
			w.APIContent, w.DisplayKind, w.DisplayMetadata = cm.APIContent, cm.DisplayKind, cm.DisplayMetadata
		}
	}
}
