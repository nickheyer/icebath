package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Prefix marking structured content stored as JSON text
const contentJSONPrefix = "\x00json:"

// Display metadata key marking a row the model sees but the person never does
const ModelOnlyKey = "model_only"

// Display kind of a row hidden from transcripts
const DisplayKindHidden = "hidden"

// One tool call an assistant message carries
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
	// Dict keys the struct has no field for, kept for lossless dict round trips
	Extra map[string]any `json:"-"`
}

// Function part of a tool call
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// Dict keys the struct has no field for, kept for lossless dict round trips
	Extra map[string]any `json:"-"`
}

// One transcript row in provider message shape
type Message struct {
	// Row id, zero before the message is stored
	RowID int64  `json:"-"`
	Role  string `json:"role"`
	// String text or a list of content parts
	Content          any             `json:"content"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolName         string          `json:"tool_name,omitempty"`
	Timestamp        float64         `json:"timestamp,omitempty"`
	TokenCount       int             `json:"token_count,omitempty"`
	FinishReason     string          `json:"finish_reason,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
	// Opaque replay items of a Responses API turn
	CodexReasoningItems json.RawMessage `json:"codex_reasoning_items,omitempty"`
	CodexMessageItems   json.RawMessage `json:"codex_message_items,omitempty"`
	PlatformMessageID   string          `json:"platform_message_id,omitempty"`
	Observed            bool            `json:"observed,omitempty"`
	CompressedSummary   bool            `json:"compressed_summary,omitempty"`
	// Exact string sent to the API when it differed from Content
	APIContent      string         `json:"api_content,omitempty"`
	DisplayKind     string         `json:"display_kind,omitempty"`
	DisplayMetadata map[string]any `json:"display_metadata,omitempty"`
	// Durable id kept across clones and compactions
	MessageUID   string            `json:"message_uid,omitempty"`
	AbsorbedUIDs []string          `json:"absorbed_message_uids,omitempty"`
	ToolCallUIDs map[string]string `json:"tool_call_uids,omitempty"`
	ToolCallUID  string            `json:"tool_call_uid,omitempty"`
	Active       bool              `json:"-"`
	Compacted    bool              `json:"-"`
	// Set once the row exists so a flush never appends it again
	Persisted bool `json:"-"`
	// Dict keys the struct has no field for, such as underscore bookkeeping keys and data_use
	Extra map[string]any `json:"-"`
}

// Text of a message, content parts joined when the content is a list
func (m *Message) Text() string {
	return ContentText(m.Content)
}

// Plain text of provider content, text parts joined with newlines
func ContentText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					parts = append(parts, t)
				}
			} else if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case []map[string]any:
		var parts []string
		for _, pm := range c {
			if t, ok := pm["text"].(string); ok {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	b, _ := json.Marshal(content)
	return string(b)
}

// Text with lone surrogates and invalid bytes replaced so SQLite can bind it
func sanitizeText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

// Content as the column stores it, structured values as prefixed JSON
func encodeContent(content any) any {
	switch c := content.(type) {
	case nil:
		return nil
	case string:
		c = sanitizeText(c)
		if !strings.HasPrefix(c, contentJSONPrefix) {
			return c
		}
		b, _ := json.Marshal(c)
		return contentJSONPrefix + string(b)
	}
	b, err := json.Marshal(content)
	if err != nil {
		return sanitizeText(fmt.Sprint(content))
	}
	return contentJSONPrefix + string(b)
}

// Content decoded from its column form
func decodeContent(raw sql.NullString) any {
	if !raw.Valid {
		return nil
	}
	if strings.HasPrefix(raw.String, contentJSONPrefix) {
		var v any
		if err := json.Unmarshal([]byte(raw.String[len(contentJSONPrefix):]), &v); err == nil {
			return v
		}
	}
	return raw.String
}

// JSON text or NULL for an optional structured column
func rawOrNull(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}

// JSON text or NULL for a string list
func listOrNull(items []string) any {
	if len(items) == 0 {
		return nil
	}
	b, _ := json.Marshal(items)
	return string(b)
}

// Stamps a durable id and per occurrence tool call ids onto a message before its first insert
func stampIdentity(m *Message, batchCalls map[string]string) {
	if m.MessageUID == "" {
		m.MessageUID = NewUID()
	}
	switch m.Role {
	case "assistant":
		if len(m.ToolCalls) > 0 {
			if m.ToolCallUIDs == nil {
				m.ToolCallUIDs = map[string]string{}
			}
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					if _, ok := m.ToolCallUIDs[tc.ID]; !ok {
						m.ToolCallUIDs[tc.ID] = NewUID()
					}
					batchCalls[tc.ID] = m.ToolCallUIDs[tc.ID]
				}
			}
		}
	case "user":
		for k := range batchCalls {
			delete(batchCalls, k)
		}
	case "tool":
		if m.ToolCallUID == "" {
			if uid, ok := batchCalls[m.ToolCallID]; ok {
				m.ToolCallUID = uid
			}
		}
	}
}

// Bound values of one message in messageColumns order
func messageParams(sessionID string, m *Message) ([]any, error) {
	var toolCalls any
	if len(m.ToolCalls) > 0 {
		b, err := json.Marshal(m.ToolCalls)
		if err != nil {
			return nil, err
		}
		toolCalls = string(b)
	}
	var displayMeta any
	if len(m.DisplayMetadata) > 0 {
		b, err := json.Marshal(m.DisplayMetadata)
		if err != nil {
			return nil, err
		}
		displayMeta = string(b)
	}
	var callUIDs any
	if len(m.ToolCallUIDs) > 0 {
		b, _ := json.Marshal(m.ToolCallUIDs)
		callUIDs = string(b)
	}
	ts := m.Timestamp
	if ts <= 0 {
		ts = now()
	}
	var tokenCount any
	if m.TokenCount > 0 {
		tokenCount = m.TokenCount
	}
	keepReasoning := m.Role == "assistant"
	reasoning := func(v string) any {
		if !keepReasoning || v == "" {
			return nil
		}
		return sanitizeText(v)
	}
	reasoningRaw := func(v json.RawMessage) any {
		if !keepReasoning {
			return nil
		}
		return rawOrNull(v)
	}
	return []any{
		sessionID, m.Role, encodeContent(m.Content), nullStr(m.ToolCallID), toolCalls, nullStr(sanitizeText(m.ToolName)), ts,
		tokenCount, nullStr(m.FinishReason), reasoning(m.Reasoning), reasoning(m.ReasoningContent),
		reasoningRaw(m.ReasoningDetails), reasoningRaw(m.CodexReasoningItems), reasoningRaw(m.CodexMessageItems),
		nullStr(m.PlatformMessageID), boolInt(m.Observed), boolInt(m.CompressedSummary), 1, 0,
		nullStr(sanitizeText(m.APIContent)), nullStr(m.DisplayKind), displayMeta, nullStr(m.MessageUID),
		listOrNull(m.AbsorbedUIDs), callUIDs, nullStr(m.ToolCallUID),
	}, nil
}

var insertMessageSQL = "INSERT INTO messages (" + strings.Join(messageColumns, ", ") + ") VALUES (" + placeholders(len(messageColumns)) + ")"

// Inserts messages as active rows and bumps the session counters, returning the row ids
func insertMessages(tx *Tx, sessionID string, msgs []*Message) ([]int64, error) {
	ids := make([]int64, 0, len(msgs))
	toolCalls := 0
	batch := map[string]string{}
	for _, m := range msgs {
		stampIdentity(m, batch)
		params, err := messageParams(sessionID, m)
		if err != nil {
			return nil, err
		}
		res, err := tx.Exec(insertMessageSQL, params...)
		if err != nil {
			return nil, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		m.RowID, m.Active, m.Persisted = id, true, true
		ids = append(ids, id)
		toolCalls += len(m.ToolCalls)
	}
	if len(msgs) > 0 {
		if _, err := tx.Exec("UPDATE sessions SET message_count = message_count + ?, tool_call_count = tool_call_count + ? WHERE id = ?",
			len(msgs), toolCalls, sessionID); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// Appends one message to a session, returning its row id
func (d *DB) AppendMessage(sessionID string, m *Message) (int64, error) {
	ids, err := d.AppendMessages(sessionID, []*Message{m})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

// Appends messages to a session in one transaction, returning their row ids
func (d *DB) AppendMessages(sessionID string, msgs []*Message) ([]int64, error) {
	var ids []int64
	err := d.transcriptWrite(func(tx *Tx) error {
		var endReason sql.NullString
		var endedAt sql.NullFloat64
		err := tx.QueryRow("SELECT ended_at, end_reason FROM sessions WHERE id = ?", sessionID).Scan(&endedAt, &endReason)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("session %s does not exist", sessionID)
		}
		if err != nil {
			return err
		}
		if endedAt.Valid && endReason.String == EndReasonCompression {
			return &CompressionSessionClosedError{SessionID: sessionID}
		}
		ids, err = insertMessages(tx, sessionID, msgs)
		return err
	})
	return ids, err
}

const messageSelect = `id, session_id, role, content, tool_call_id, tool_calls, tool_name, timestamp, token_count, finish_reason,
	reasoning, reasoning_content, reasoning_details, codex_reasoning_items, codex_message_items, platform_message_id,
	observed, _compressed_summary, active, compacted, api_content, display_kind, display_metadata, message_uid,
	absorbed_message_uids, tool_call_uids, tool_call_uid`

// Message decoded from a row selected with messageSelect
func scanMessage(r rowScanner) (*Message, string, error) {
	var m Message
	var sessionID string
	var content, toolCallID, toolCalls, toolName, finish, reasoning, reasoningContent, details, codexReasoning, codexMessages, platformID, apiContent, displayKind, displayMeta, uid, absorbed, callUIDs, callUID sql.NullString
	var tokenCount sql.NullInt64
	var observed, summary, active, compacted int
	err := r.Scan(&m.RowID, &sessionID, &m.Role, &content, &toolCallID, &toolCalls, &toolName, &m.Timestamp, &tokenCount, &finish,
		&reasoning, &reasoningContent, &details, &codexReasoning, &codexMessages, &platformID, &observed, &summary,
		&active, &compacted, &apiContent, &displayKind, &displayMeta, &uid, &absorbed, &callUIDs, &callUID)
	if err != nil {
		return nil, "", err
	}
	m.Content = decodeContent(content)
	m.ToolCallID, m.ToolName, m.FinishReason = toolCallID.String, toolName.String, finish.String
	m.Reasoning, m.ReasoningContent = reasoning.String, reasoningContent.String
	m.PlatformMessageID, m.APIContent, m.DisplayKind = platformID.String, apiContent.String, displayKind.String
	m.MessageUID, m.ToolCallUID = uid.String, callUID.String
	m.TokenCount = int(tokenCount.Int64)
	m.Observed, m.CompressedSummary = observed != 0, summary != 0
	m.Active, m.Compacted, m.Persisted = active != 0, compacted != 0, true
	if toolCalls.Valid && toolCalls.String != "" {
		if err := json.Unmarshal([]byte(toolCalls.String), &m.ToolCalls); err != nil {
			m.ToolCalls = nil
		}
	}
	if details.Valid && details.String != "" && json.Valid([]byte(details.String)) {
		m.ReasoningDetails = json.RawMessage(details.String)
	}
	if codexReasoning.Valid && codexReasoning.String != "" && json.Valid([]byte(codexReasoning.String)) {
		m.CodexReasoningItems = json.RawMessage(codexReasoning.String)
	}
	if codexMessages.Valid && codexMessages.String != "" && json.Valid([]byte(codexMessages.String)) {
		m.CodexMessageItems = json.RawMessage(codexMessages.String)
	}
	if displayMeta.Valid && displayMeta.String != "" {
		var meta map[string]any
		if err := json.Unmarshal([]byte(displayMeta.String), &meta); err == nil {
			m.DisplayMetadata = meta
		}
	}
	if absorbed.Valid && absorbed.String != "" {
		_ = json.Unmarshal([]byte(absorbed.String), &m.AbsorbedUIDs)
	}
	if callUIDs.Valid && callUIDs.String != "" {
		_ = json.Unmarshal([]byte(callUIDs.String), &m.ToolCallUIDs)
	}
	return &m, sessionID, nil
}

// Visibility predicate of a transcript read
func activeClause(includeInactive, includeCompacted bool) string {
	switch {
	case includeInactive:
		return ""
	case includeCompacted:
		return " AND (active = 1 OR compacted = 1)"
	}
	return " AND active = 1"
}

// Rows of a session in id order under a visibility predicate
func (d *DB) fetchMessages(sessionIDs []string, clause string) ([]*Message, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	var out []*Message
	err := d.read(func(q querier) error {
		rows, err := q.Query("SELECT "+messageSelect+" FROM messages WHERE session_id IN ("+placeholders(len(sessionIDs))+")"+clause+" ORDER BY id", args(sessionIDs)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			m, _, err := scanMessage(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	pairToolCallUIDs(out)
	return out, err
}

// Fills the tool call uid of result rows older builds left unpaired from the preceding assistant row
func pairToolCallUIDs(msgs []*Message) {
	index := map[string]string{}
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			for id, uid := range m.ToolCallUIDs {
				index[id] = uid
			}
		case "user":
			index = map[string]string{}
		case "tool":
			if m.ToolCallUID == "" && m.ToolCallID != "" {
				m.ToolCallUID = index[m.ToolCallID]
			}
		}
	}
}

// Live messages of a session, the exact transcript the model is fed
func (d *DB) Conversation(sessionID string) ([]*Message, error) {
	return d.fetchMessages([]string{sessionID}, activeClause(false, false))
}

// Every stored row of a session including rewound and compacted ones
func (d *DB) AllMessages(sessionID string) ([]*Message, error) {
	return d.fetchMessages([]string{sessionID}, "")
}

// Messages the person sees, live rows plus compacted history with carried copies shown once
func (d *DB) DisplayMessages(sessionID string) ([]*Message, error) {
	rows, err := d.fetchMessages([]string{sessionID}, activeClause(false, true))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []*Message
	for _, m := range rows {
		if m.DisplayMetadata != nil {
			if v, ok := m.DisplayMetadata[ModelOnlyKey]; ok && v != false && v != nil && v != 0 {
				continue
			}
		}
		key := displayIdentity(m)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	return out, nil
}

// Identity collapsing a message and its carried copies for display
func displayIdentity(m *Message) string {
	if m.MessageUID != "" {
		return "uid:" + m.MessageUID
	}
	calls, _ := json.Marshal(m.ToolCalls)
	return fmt.Sprintf("%s|%s|%.3f|%s|%s", m.Role, ContentText(m.Content), m.Timestamp, m.ToolCallID, calls)
}

// Highest active row id of a session, zero when empty
func (d *DB) ActiveWatermark(sessionID string) (int64, error) {
	var mark sql.NullInt64
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT MAX(id) FROM messages WHERE session_id = ? AND active = 1", sessionID).Scan(&mark)
	})
	return mark.Int64, err
}

// Role of the newest live row of a session, empty when none
func (d *DB) LatestRole(sessionID string) (string, error) {
	var role string
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT role FROM messages WHERE session_id = ? AND active = 1 ORDER BY id DESC LIMIT 1", sessionID).Scan(&role)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// Number of live rows of a session
func (d *DB) ActiveMessageCount(sessionID string) (int, error) {
	var n int
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id = ? AND active = 1", sessionID).Scan(&n)
	})
	return n, err
}

// Replaces the live transcript of a session, old rows kept as rewound history
func (d *DB) ReplaceMessages(sessionID string, msgs []*Message) error {
	return d.transcriptWrite(func(tx *Tx) error {
		if _, err := tx.Exec("UPDATE messages SET active = 0 WHERE session_id = ? AND active = 1", sessionID); err != nil {
			return err
		}
		for _, m := range msgs {
			m.RowID, m.Persisted = 0, false
		}
		if _, err := insertMessages(tx, sessionID, msgs); err != nil {
			return err
		}
		return resetCounters(tx, sessionID)
	})
}

// Recomputes the row counters of a session from its live rows
func resetCounters(tx *Tx, sessionID string) error {
	_, err := tx.Exec(`UPDATE sessions SET
		message_count = (SELECT COUNT(*) FROM messages m WHERE m.session_id = sessions.id AND m.active = 1),
		tool_call_count = (SELECT COALESCE(SUM(CASE WHEN m.tool_calls IS NULL THEN 0 ELSE json_array_length(m.tool_calls) END), 0)
			FROM messages m WHERE m.session_id = sessions.id AND m.active = 1 AND m.role = 'assistant')
		WHERE id = ?`, sessionID)
	return err
}

// Result of an in place compaction
type CompactResult struct {
	// Live rows after the compaction
	ActiveCount int
	// Rows that arrived after the watermark and were cloned behind the compacted set
	Cloned int
}

// Archives the live rows of a session and installs the compacted set, rows written after the watermark cloned behind it
func (d *DB) ArchiveAndCompact(sessionID string, compacted []*Message, watermark int64, lockHolder string, tailCount int) (CompactResult, error) {
	var result CompactResult
	err := d.transcriptWrite(func(tx *Tx) error {
		if lockHolder != "" {
			var holder string
			var expires float64
			err := tx.QueryRow("SELECT holder, expires_at FROM compression_locks WHERE session_id = ?", sessionID).Scan(&holder, &expires)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && (holder != lockHolder || expires <= now())) {
				return &SessionCompressionInProgressError{Msg: "Compression lease for " + pyRepr(sessionID) + " lost before commit; refusing to publish a stale compaction"}
			}
			if err != nil {
				return err
			}
		}
		var tailIDs []int64
		if watermark > 0 {
			rows, err := tx.Query("SELECT id FROM messages WHERE session_id = ? AND active = 1 AND id > ? ORDER BY id", sessionID, watermark)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				tailIDs = append(tailIDs, id)
			}
			rows.Close()
		}
		carried := map[string]bool{}
		for _, m := range compacted {
			if m.MessageUID != "" {
				carried[m.MessageUID] = true
			}
		}
		var cloneIDs []int64
		for _, id := range tailIDs {
			var uid sql.NullString
			if err := tx.QueryRow("SELECT message_uid FROM messages WHERE id = ?", id).Scan(&uid); err != nil {
				return err
			}
			if uid.Valid && carried[uid.String] {
				continue
			}
			cloneIDs = append(cloneIDs, id)
		}
		rewind := map[int64]bool{}
		for _, id := range tailIDs {
			rewind[id] = true
		}
		if tailCount > 0 {
			bound := ""
			params := []any{sessionID}
			if watermark > 0 {
				bound = " AND id <= ?"
				params = append(params, watermark)
			}
			params = append(params, tailCount)
			rows, err := tx.Query("SELECT id FROM messages WHERE session_id = ? AND active = 1"+bound+" ORDER BY id DESC LIMIT ?", params...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				rewind[id] = true
			}
			rows.Close()
		}
		if len(rewind) > 0 {
			ids := make([]any, 0, len(rewind)+1)
			ids = append(ids, sessionID)
			for id := range rewind {
				ids = append(ids, id)
			}
			ph := placeholders(len(rewind))
			if _, err := tx.Exec("UPDATE messages SET active = 0, compacted = 0 WHERE session_id = ? AND id IN ("+ph+")", ids...); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE messages SET active = 0, compacted = 1 WHERE session_id = ? AND active = 1 AND id NOT IN ("+ph+")", ids...); err != nil {
				return err
			}
		} else if _, err := tx.Exec("UPDATE messages SET active = 0, compacted = 1 WHERE session_id = ? AND active = 1", sessionID); err != nil {
			return err
		}
		for _, m := range compacted {
			m.RowID, m.Persisted = 0, false
		}
		if _, err := insertMessages(tx, sessionID, compacted); err != nil {
			return err
		}
		for _, id := range cloneIDs {
			if _, err := tx.Exec(`INSERT INTO messages (`+strings.Join(messageColumns, ", ")+`)
				SELECT `+strings.Join(messageColumns, ", ")+` FROM messages WHERE id = ?`, id); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE messages SET active = 1, compacted = 0 WHERE id = last_insert_rowid()"); err != nil {
				return err
			}
		}
		result.Cloned = len(cloneIDs)
		if err := resetCounters(tx, sessionID); err != nil {
			return err
		}
		return tx.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id = ? AND active = 1", sessionID).Scan(&result.ActiveCount)
	})
	return result, err
}

// Outcome of a rewind
type RewindResult struct {
	RewoundCount int
	Target       *Message
	NewHeadID    int64
}

// Deactivates the target user row and everything after it, returning the target for a re-prompt
func (d *DB) RewindToMessage(sessionID string, targetRowID int64) (RewindResult, error) {
	var result RewindResult
	err := d.transcriptWrite(func(tx *Tx) error {
		target, _, err := scanMessage(tx.QueryRow("SELECT "+messageSelect+" FROM messages WHERE session_id = ? AND id = ? AND active = 1", sessionID, targetRowID))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("message %d is not a live row of session %s", targetRowID, sessionID)
		}
		if err != nil {
			return err
		}
		if target.Role != "user" {
			return fmt.Errorf("message %d is a %s row, only user rows can be rewound to", targetRowID, target.Role)
		}
		res, err := tx.Exec("UPDATE messages SET active = 0, compacted = 0 WHERE session_id = ? AND active = 1 AND id >= ?", sessionID, targetRowID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		result.RewoundCount, result.Target = int(n), target
		var head sql.NullInt64
		if err := tx.QueryRow("SELECT MAX(id) FROM messages WHERE session_id = ? AND active = 1", sessionID).Scan(&head); err != nil {
			return err
		}
		result.NewHeadID = head.Int64
		if _, err := tx.Exec("UPDATE sessions SET rewind_count = rewind_count + 1 WHERE id = ?", sessionID); err != nil {
			return err
		}
		return resetCounters(tx, sessionID)
	})
	return result, err
}

// Deactivates one live row, reporting whether it existed
func (d *DB) DeactivateMessage(sessionID string, rowID int64) (bool, error) {
	changed := false
	err := d.write(func(tx *Tx) error {
		res, err := tx.Exec("UPDATE messages SET active = 0, compacted = 0 WHERE session_id = ? AND id = ? AND active = 1", sessionID, rowID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		changed = n > 0
		return resetCounters(tx, sessionID)
	})
	return changed, err
}

// Rewrites the content of a user row in place
func (d *DB) SetMessageContent(sessionID string, rowID int64, content any) error {
	return d.write(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE messages SET content = ? WHERE session_id = ? AND id = ?", encodeContent(content), sessionID, rowID)
		return err
	})
}

// Row id of the newest live row of a role, zero when none
func (d *DB) LatestRowID(sessionID, role string) (int64, error) {
	var id sql.NullInt64
	err := d.read(func(q querier) error {
		return q.QueryRow("SELECT MAX(id) FROM messages WHERE session_id = ? AND active = 1 AND role = ?", sessionID, role).Scan(&id)
	})
	return id.Int64, err
}

// Deletes every row of a session and zeroes its counters
func (d *DB) ClearMessages(sessionID string) error {
	return d.write(func(tx *Tx) error {
		if _, err := tx.Exec("DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE sessions SET message_count = 0, tool_call_count = 0 WHERE id = ?", sessionID)
		return err
	})
}

// Rows of a session with the rows around one row id, for search context
func (d *DB) MessagesAround(sessionID string, rowID int64, window int) ([]*Message, error) {
	var out []*Message
	err := d.read(func(q querier) error {
		rows, err := q.Query("SELECT "+messageSelect+" FROM messages WHERE session_id = ? AND (active = 1 OR compacted = 1) AND id BETWEEN ? AND ? ORDER BY id",
			sessionID, rowID-int64(window), rowID+int64(window))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, _, err := scanMessage(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// Column list every message writer binds, in insert order
var messageColumns = []string{
	"session_id", "role", "content", "tool_call_id", "tool_calls", "tool_name", "timestamp", "token_count",
	"finish_reason", "reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items",
	"codex_message_items", "platform_message_id", "observed", "_compressed_summary", "active", "compacted",
	"api_content", "display_kind", "display_metadata", "message_uid", "absorbed_message_uids",
	"tool_call_uids", "tool_call_uid",
}
