package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Error of paging options that cannot be combined
var ErrInvalidPaging = errors.New("invalid message paging")

// Error of a read that needs alternation repair on a store opened without the agent's repairer
var ErrNoSequenceRepair = errors.New("store was opened without a message sequence repairer")

// One stored messages row as a transcript read returns it
type MessageRow struct {
	ID        int64
	SessionID string
	Role      string
	Content   Content
	// Stored tool calls JSON, nil when the row has none and an empty list when the stored text is not JSON
	ToolCalls         json.RawMessage
	ToolCallID        sql.Null[string]
	ToolName          sql.Null[string]
	EffectDisposition sql.Null[string]
	Timestamp         sql.Null[float64]
	TokenCount        sql.Null[int64]
	FinishReason      sql.Null[string]
	Reasoning         sql.Null[string]
	ReasoningContent  sql.Null[string]
	// Structured reasoning carriers as their stored text
	ReasoningDetails    sql.Null[string]
	CodexReasoningItems sql.Null[string]
	CodexMessageItems   sql.Null[string]
	PlatformMessageID   sql.Null[string]
	Observed            bool
	CompressedSummary   bool
	Active              bool
	Compacted           bool
	APIContent          sql.Null[string]
	DisplayKind         sql.Null[string]
	// Display metadata object, nil when absent or not an object
	DisplayMetadata     json.RawMessage
	MessageUID          sql.Null[string]
	AbsorbedMessageUIDs sql.Null[string]
	ToolCallUIDs        sql.Null[string]
	ToolCallUID         sql.Null[string]
}

// Typed row of raw cells with content, tool calls and display metadata decoded
func rowToMessageRow(r *msgRow, warnContext string) MessageRow {
	out := MessageRow{ID: r.id, SessionID: r.session(), Role: r.roleText(), Content: decodeContent(r.content),
		ToolCallID: nullCell(r.toolCallID), ToolName: nullCell(r.toolName), EffectDisposition: nullCell(r.effectDisposition),
		FinishReason: nullCell(r.finishReason), Reasoning: nullCell(r.reasoning), ReasoningContent: nullCell(r.reasoningContent),
		ReasoningDetails: nullCell(r.reasoningDetails), CodexReasoningItems: nullCell(r.codexReasoningItems),
		CodexMessageItems: nullCell(r.codexMessageItems), PlatformMessageID: nullCell(r.platformMessageID),
		Observed: cellTruthy(r.observed), CompressedSummary: cellTruthy(r.compressedSummary), Active: cellTruthy(r.active),
		Compacted: cellTruthy(r.compacted), APIContent: nullCell(r.apiContent), DisplayKind: nullCell(r.displayKind),
		MessageUID: nullCell(r.messageUID), AbsorbedMessageUIDs: nullCell(r.absorbedMessageUIDs),
		ToolCallUIDs: nullCell(r.toolCallUIDs), ToolCallUID: nullCell(r.toolCallUID)}
	if ts, ok := cellFloat(r.timestamp); ok {
		out.Timestamp = sql.Null[float64]{V: ts, Valid: true}
	}
	if n, ok := cellInt(r.tokenCount); ok && r.tokenCount != nil {
		out.TokenCount = sql.Null[int64]{V: n, Valid: true}
	}
	if cellTruthy(r.toolCalls) {
		if out.ToolCalls = jsonCell(r.toolCalls, "Failed to deserialize tool_calls in "+warnContext+", falling back to []"); out.ToolCalls == nil {
			out.ToolCalls = json.RawMessage("[]")
		}
	}
	if r.displayMetadata != nil {
		out.DisplayMetadata = decodeDisplayMetadata(r.displayMetadata)
	}
	return out
}

// Typed rows of raw rows
func rowsToMessageRows(rows []*msgRow, warnContext string) []MessageRow {
	out := make([]MessageRow, len(rows))
	for i, r := range rows {
		out[i] = rowToMessageRow(r, warnContext)
	}
	return out
}

// Which rows and which page of them a transcript read loads
type MessageQuery struct {
	// Every row, rewound ones included
	IncludeInactive bool
	// Live rows plus compaction archived display history, deduped across generations
	IncludeCompacted bool
	// Rows per page, nil reading all
	Limit  *int
	Offset int
	// Pages back from the newest while returning chronological order
	Latest bool
	// Keyset paging, only rows with a greater id
	AfterID *int64
	// Also loads the compression lineage root to tip
	IncludeAncestors bool
}

// Row filter of a read, every row for an audit, live plus compaction archived for display, live otherwise
func activeClause(includeInactive, includeCompacted bool) string {
	switch {
	case includeInactive:
		return ""
	case includeCompacted:
		return displayActiveClause
	}
	return " AND active = 1"
}

// Message rows of one read query
func (s *Store) readMsgRows(ctx context.Context, query string, args ...any) ([]*msgRow, error) {
	var out []*msgRow
	err := s.Read(ctx, func(c Conn) error {
		var err error
		out, err = queryMsgRows(c.QueryContext(ctx, query, args...))
		return err
	})
	return out, err
}

// Loads messages in insertion order under the paging and visibility the query names
func (s *Store) GetMessages(ctx context.Context, sessionID string, q MessageQuery) ([]MessageRow, error) {
	if q.AfterID != nil {
		switch {
		case q.Latest || q.Offset != 0:
			return nil, fmt.Errorf("%w: after_id is incompatible with latest/offset paging", ErrInvalidPaging)
		case q.IncludeCompacted:
			return nil, fmt.Errorf("%w: after_id is incompatible with include_compacted (deduped display reads use offset paging)", ErrInvalidPaging)
		case q.IncludeAncestors:
			return nil, fmt.Errorf("%w: after_id is incompatible with include_ancestors (merged-lineage reads use offset paging)", ErrInvalidPaging)
		}
	}
	clause := activeClause(q.IncludeInactive, q.IncludeCompacted)
	ids := []string{sessionID}
	if q.IncludeAncestors {
		var err error
		if ids, err = s.ResumeLineageIDs(ctx, sessionID); err != nil {
			return nil, err
		}
	}
	var rows []*msgRow
	var err error
	display := q.IncludeCompacted && !q.IncludeInactive
	switch {
	case len(ids) > 1:
		if rows, err = s.readMsgRows(ctx, "SELECT * FROM messages WHERE session_id IN ("+bindMarks(len(ids))+")"+clause+" ORDER BY id ASC",
			stringArgs(ids)...); err != nil {
			return nil, err
		}
		if display {
			if rows, err = s.withRemovedIdentities(ctx, rows, ids); err != nil {
				return nil, err
			}
		}
		rows = pageRows(dedupeDisplayGenerations(rows, display), q.Offset, q.Limit, q.Latest)
	case display:
		indexed, err := s.ensureDisplayOrder(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		if indexed {
			err = s.Read(ctx, func(c Conn) error {
				var err error
				rows, err = displayRowsIn(ctx, c, sessionID, q.Limit, q.Offset, q.Latest)
				return err
			})
		} else {
			rows, err = s.legacyDisplayPage(ctx, sessionID, clause, q.Limit, q.Offset, q.Latest)
		}
		if err != nil {
			return nil, err
		}
	case q.IncludeCompacted:
		if rows, err = s.legacyDisplayPage(ctx, sessionID, clause, q.Limit, q.Offset, q.Latest); err != nil {
			return nil, err
		}
	default:
		query := "SELECT * FROM messages WHERE session_id = ?" + clause
		args := []any{sessionID}
		if q.AfterID != nil {
			query += " AND id > ?"
			args = append(args, *q.AfterID)
		}
		query += " ORDER BY id ASC"
		if q.Latest {
			query = strings.TrimSuffix(query, "ASC") + "DESC"
		}
		if q.Limit != nil || q.Offset != 0 {
			bound := -1
			if q.Limit != nil {
				bound = *q.Limit
			}
			query += " LIMIT ? OFFSET ?"
			args = append(args, bound, q.Offset)
		}
		if rows, err = s.readMsgRows(ctx, query, args...); err != nil {
			return nil, err
		}
		if q.Latest {
			for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	return rowsToMessageRows(rows, "get_messages"), nil
}

// Session and content of one tool result that mentions a pull request path
type PRURLMessage struct {
	SessionID string
	Content   string
}

// Tool results containing a pull request path across the sessions, oldest first
func (s *Store) FindPRURLMessages(ctx context.Context, sessionIDs []string) ([]PRURLMessage, error) {
	var ids []string
	for _, id := range sessionIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	var out []PRURLMessage
	for _, chunk := range chunks(ids, sqlInChunk) {
		err := s.Read(ctx, func(c Conn) error {
			rows, err := c.QueryContext(ctx, `SELECT session_id, content FROM messages
                    WHERE session_id IN (`+bindMarks(len(chunk))+`)
                      AND role = 'tool' AND content LIKE '%/pull/%'
                    ORDER BY id ASC`, stringArgs(chunk)...)
			if err != nil {
				return err
			}
			defer rows.Close()
			var page []PRURLMessage
			for rows.Next() {
				var m PRURLMessage
				if err := rows.Scan(lenient{&m.SessionID}, lenient{&m.Content}); err != nil {
					return err
				}
				page = append(page, m)
			}
			out = append(out, page...)
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Messages around an anchor and how many lie strictly before and after it inside the window
type MessageWindow struct {
	Window         []MessageRow
	MessagesBefore int
	MessagesAfter  int
}

// Up to window messages either side of an anchor id in ascending order, empty for a foreign anchor
func (s *Store) GetMessagesAround(ctx context.Context, sessionID string, aroundMessageID int64, window int) (MessageWindow, error) {
	window = max(window, 0)
	var out MessageWindow
	err := s.Read(ctx, func(c Conn) error {
		out = MessageWindow{}
		var one int
		err := c.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE id = ? AND session_id = ? LIMIT 1", aroundMessageID, sessionID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		before, err := queryMsgRows(c.QueryContext(ctx, "SELECT * FROM messages WHERE session_id = ? AND id <= ? ORDER BY id DESC LIMIT ?",
			sessionID, aroundMessageID, window+1))
		if err != nil {
			return err
		}
		after, err := queryMsgRows(c.QueryContext(ctx, "SELECT * FROM messages WHERE session_id = ? AND id > ? ORDER BY id ASC LIMIT ?",
			sessionID, aroundMessageID, window))
		if err != nil {
			return err
		}
		for i := len(before) - 1; i >= 0; i-- {
			out.Window = append(out.Window, rowToMessageRow(before[i], "get_messages_around"))
		}
		out.Window = append(out.Window, rowsToMessageRows(after, "get_messages_around")...)
		out.MessagesBefore, out.MessagesAfter = max(0, len(before)-1), len(after)
		return nil
	})
	return out, err
}

// Redirects a resume target to the descendant holding the messages, the compression tip and then the deepest continuation with rows
func (s *Store) ResolveResumeSessionID(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return sessionID, nil
	}
	if tip, err := s.CompressionTip(ctx, sessionID); err == nil && tip != "" {
		sessionID = tip
	}
	best := sessionID
	err := s.Read(ctx, func(c Conn) error {
		best = sessionID
		current, found := sessionID, ""
		seen := map[string]bool{current: true}
		for range 32 {
			var one int
			err := c.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE session_id = ? LIMIT 1", current).Scan(&one)
			if err == nil {
				found = current
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			var child sql.NullString
			err = c.QueryRowContext(ctx, "SELECT id FROM sessions AS child WHERE child.parent_session_id = ? "+
				"  AND "+JSONExtractSQL("child.model_config", "$._branched_from")+" IS NULL "+
				"  AND "+JSONExtractSQL("child.model_config", "$._delegate_from")+" IS NULL "+
				"  AND "+JSONExtractSQL("child.model_config", "$._reset_from")+" IS NULL "+
				"  AND NOT "+legacyResetChildSQL("child", sqlLiteralList(resetEndReasons))+" "+
				"  AND COALESCE(child.source, '') != 'tool' "+
				"ORDER BY child.started_at DESC, child.id DESC LIMIT 1", current).Scan(&child)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return nil
			}
			if child.String == "" || seen[child.String] {
				break
			}
			current = child.String
			seen[current] = true
		}
		if found != "" {
			best = found
		}
		return nil
	})
	return best, err
}

// Columns every conversation projection decodes
const conversationRowColumns = "id, role, content, tool_call_id, tool_calls, tool_name, effect_disposition, " +
	"finish_reason, reasoning, reasoning_content, reasoning_details, " +
	"codex_reasoning_items, codex_message_items, platform_message_id, observed, " +
	"_compressed_summary, timestamp, token_count, active, api_content, display_kind, display_metadata, message_uid, " +
	"absorbed_message_uids, tool_call_uids, tool_call_uid"

// Conversation rows of the sessions in id order, the display set with removed identities when compacted rows are wanted
func (s *Store) fetchConversationRows(ctx context.Context, sessionIDs []string, clause string, withSessionID, withCompacted bool) ([]*msgRow, error) {
	head := ""
	if withSessionID || withCompacted {
		head = "session_id, "
	}
	if withCompacted {
		head += "compacted, display_order, "
		clause = displayActiveClause
	}
	rows, err := s.readMsgRows(ctx, "SELECT "+head+conversationRowColumns+" FROM messages WHERE session_id IN ("+bindMarks(len(sessionIDs))+")"+
		clause+" ORDER BY id", stringArgs(sessionIDs)...)
	if err != nil || !withCompacted {
		return rows, err
	}
	return s.withRemovedIdentities(ctx, rows, sessionIDs)
}

// Rows merged in id order with the identities of the lineage's removed rows, which only ever hide a copy
func (s *Store) withRemovedIdentities(ctx context.Context, rows []*msgRow, sessionIDs []string) ([]*msgRow, error) {
	removed, err := s.readMsgRows(ctx, "SELECT session_id, id, role, timestamp, tool_call_id, tool_calls, tool_name, display_kind, display_metadata, "+
		"message_uid, absorbed_message_uids, display_identity, active, compacted, "+
		"CASE WHEN display_identity IS NULL THEN content END AS content "+
		"FROM messages WHERE session_id IN ("+bindMarks(len(sessionIDs))+") AND active = 0 AND compacted = 0 "+
		"ORDER BY id", stringArgs(sessionIDs)...)
	if err != nil || len(removed) == 0 {
		return rows, err
	}
	merged := make([]*msgRow, 0, len(rows)+len(removed))
	i, j := 0, 0
	for i < len(rows) || j < len(removed) {
		if j >= len(removed) || (i < len(rows) && rows[i].id <= removed[j].id) {
			merged = append(merged, rows[i])
			i++
		} else {
			merged = append(merged, removed[j])
			j++
		}
	}
	return merged, nil
}

// What a conversation read projects and repairs
type ConversationQuery struct {
	// Loads the whole compression lineage root to tip
	IncludeAncestors bool
	// Every row, rewound ones included
	IncludeInactive bool
	// Repairs role alternation for a live replay and stamps the version digest and summary markers it needs
	RepairAlternation bool
	// Stamps each message with the id of its row
	IncludeRowIDs bool
	// Deduped display history with compaction archived rows, for display reads only
	IncludeCompacted bool
}

// Loads a session's messages in provider conversation form
func (s *Store) GetMessagesAsConversation(ctx context.Context, sessionID string, q ConversationQuery) ([]*Message, error) {
	display := q.IncludeCompacted && !q.IncludeInactive
	ids := []string{sessionID}
	if q.IncludeAncestors {
		var err error
		if ids, err = s.ResumeLineageIDs(ctx, sessionID); err != nil {
			return nil, err
		}
	}
	rows, err := s.fetchConversationRows(ctx, ids, activeClause(q.IncludeInactive, q.IncludeCompacted), false, display)
	if err != nil {
		return nil, err
	}
	if q.IncludeCompacted {
		rows = dedupeDisplayGenerations(rows, display)
	}
	return s.rowsToConversation(rows, conversionOptions{sessionID: sessionID, includeAncestors: q.IncludeAncestors,
		repairAlternation: q.RepairAlternation, includeRowIDs: q.IncludeRowIDs, includeSummaryMarkers: q.RepairAlternation})
}

// Fingerprint of what a live conversation read would load, moved by any append, rewind, compaction or rewrite
type ConversationRevision struct {
	SessionIDs []string
	// Active rows and the highest active id across the sessions
	ActiveCount int64
	MaxID       int64
	// Sum of the sessions' write side transcript revisions
	TranscriptRevision int64
}

// Cheap fingerprint of the live transcript a conversation read would load
func (s *Store) ConversationRevision(ctx context.Context, sessionID string, includeAncestors bool) (ConversationRevision, error) {
	ids := []string{sessionID}
	if includeAncestors {
		var err error
		if ids, err = s.ResumeLineageIDs(ctx, sessionID); err != nil {
			return ConversationRevision{}, err
		}
	}
	rev := ConversationRevision{SessionIDs: ids}
	marks := bindMarks(len(ids))
	err := s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(id), 0), (SELECT COALESCE(SUM(transcript_revision), 0) FROM sessions"+
			" WHERE id IN ("+marks+")) FROM messages WHERE session_id IN ("+marks+") AND active = 1",
			append(stringArgs(ids), stringArgs(ids)...)...).Scan(&rev.ActiveCount, &rev.MaxID, &rev.TranscriptRevision)
	})
	return rev, err
}

// How fetched rows become conversation messages
type conversionOptions struct {
	sessionID             string
	includeAncestors      bool
	repairAlternation     bool
	includeRowIDs         bool
	includeSummaryMarkers bool
}

// Openings of the background review harness prompts older builds wrote into real sessions
var reviewHarnessPrefixes = []string{
	"Review the conversation above and update the skill library",
	"Review the conversation above and consider saving to memory",
}

// Bare protocol or tool name marker such as [memory]
var staleToolCallMarker = regexp.MustCompile(`^\[[A-Za-z_][A-Za-z0-9_.-]*\]$`)

// Reports a persisted background review harness prompt
func isReviewHarnessMessage(m *Message) bool {
	if m.Role != "user" && m.Role != "system" {
		return false
	}
	text, ok := m.Content.Text()
	if !ok {
		return false
	}
	text = strings.TrimLeftFunc(text, previewSpace)
	for _, p := range reviewHarnessPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// Messages without harness prompts and the curator reply that immediately followed each
func stripReviewHarness(messages []*Message) []*Message {
	out := make([]*Message, 0, len(messages))
	skipNextAssistant, previousWasHarness := false, false
	for _, m := range messages {
		if isReviewHarnessMessage(m) {
			skipNextAssistant = !previousWasHarness
			previousWasHarness = true
			continue
		}
		if skipNextAssistant {
			skipNextAssistant = false
			if m.Role == "assistant" {
				previousWasHarness = false
				continue
			}
		}
		previousWasHarness = false
		out = append(out, m)
	}
	return out
}

// Blanks assistant tool call turns whose content is a bare marker so a replay cannot teach the model to emit it
func stripStaleToolCallMarkers(messages []*Message) {
	repaired := 0
	for _, m := range messages {
		if m.Role != "assistant" || !jsonTruthy(m.ToolCalls) {
			continue
		}
		text, ok := m.Content.Text()
		if ok && staleToolCallMarker.MatchString(strings.TrimFunc(text, previewSpace)) {
			m.Content = TextContent("")
			repaired++
		}
	}
	if repaired > 0 {
		slog.Info(fmt.Sprintf("Cleared %d stale tool-call marker message(s) while restoring session (#78148)", repaired))
	}
}

// Canonical live content of a replayed user message and whether it is a composite summary carrier
func canonicalReplayedUserContent(m *Message) (Content, bool) {
	if m.Role != "user" {
		return Content{}, false
	}
	split := splitUserOriginatedTurn(userTurn{content: m.Content, displayKind: m.DisplayKind.V, summary: m.CompressedSummary})
	if split.handoff && split.live {
		return split.liveContent, true
	}
	return m.Content, false
}

// Order insensitive comparison text of content, empty for absent, empty text and an empty list
func contentCompareKey(c Content) string {
	switch {
	case c.IsNull():
		return ""
	case c.kind == contentText:
		if c.text == "" {
			return ""
		}
		return "t:" + c.text
	}
	enc, err := c.MarshalJSON()
	if err != nil {
		return ""
	}
	var v any
	if json.Unmarshal(enc, &v) != nil {
		return "r:" + string(enc)
	}
	if list, ok := v.([]any); ok && len(list) == 0 {
		return ""
	}
	sorted, err := json.Marshal(v)
	if err != nil {
		return "r:" + string(enc)
	}
	return "j:" + string(sorted)
}

// Adjacent replay duplicate of a user message among the messages already loaded, its index and whether the current copy wins
func findDuplicateReplayedUser(messages []*Message, m *Message) (int, bool, bool) {
	if m.Role != "user" {
		return 0, false, false
	}
	content, preferCurrent := canonicalReplayedUserContent(m)
	key := contentCompareKey(content)
	if key == "" {
		return 0, false, false
	}
	_, isText := content.Text()
	for i := len(messages) - 1; i >= 0; i-- {
		prev := messages[i]
		switch prev.Role {
		case "user":
			prevContent, prevComposite := canonicalReplayedUserContent(prev)
			if contentCompareKey(prevContent) == key && (preferCurrent || prevComposite || isText) {
				return i, preferCurrent, true
			}
		case "assistant":
			if prev.Content.truthy() || jsonTruthy(prev.ToolCalls) {
				return 0, false, false
			}
		}
	}
	return 0, false, false
}

// Decodes fetched rows in id order into conversation messages, every one stamped durable
func (s *Store) rowsToConversation(rows []*msgRow, o conversionOptions) ([]*Message, error) {
	stampSnapshot := o.repairAlternation && !o.includeRowIDs
	messages := make([]*Message, 0, len(rows))
	exactClones := map[string]*Message{}
	toolIndex := map[string]string{}
	var unindexedOwners []*Message
	for _, r := range rows {
		role := r.roleText()
		m := &Message{Role: role, Content: loadedViewContent(role, decodeContent(r.content)), Persisted: true}
		if stampSnapshot {
			m.Snapshot = rowSnapshot(r)
		}
		if o.includeRowIDs {
			m.RowID = r.id
		}
		restoreIdentityColumns(r, m)
		if cellTruthy(r.apiContent) {
			m.APIContent = nullCell(r.apiContent)
		}
		if cellTruthy(r.displayKind) {
			m.DisplayKind = nullCell(r.displayKind)
		}
		if cellTruthy(r.displayMetadata) {
			if decoded := decodeDisplayMetadata(r.displayMetadata); decoded != nil {
				m.DisplayMetadata = decoded
				liftDataUse(m)
			}
		}
		if o.includeSummaryMarkers && cellTruthy(r.compressedSummary) {
			m.CompressedSummary = true
		}
		if ts, ok := cellFloat(r.timestamp); ok && cellTruthy(r.timestamp) {
			m.Timestamp = sql.Null[float64]{V: ts, Valid: true}
		}
		if cellTruthy(r.toolCallID) {
			m.ToolCallID = nullCell(r.toolCallID)
		}
		if cellTruthy(r.toolName) {
			m.ToolName = nullCell(r.toolName)
		}
		if cellTruthy(r.effectDisposition) {
			m.EffectDisposition = nullCell(r.effectDisposition)
		}
		if cellTruthy(r.toolCalls) {
			if m.ToolCalls = jsonCell(r.toolCalls, "Failed to deserialize tool_calls in conversation replay, falling back to []"); m.ToolCalls == nil {
				m.ToolCalls = json.RawMessage("[]")
			}
		}
		if cellTruthy(r.platformMessageID) {
			m.PlatformMessageID = nullCell(r.platformMessageID)
		}
		m.Observed = cellTruthy(r.observed)
		switch role {
		case "assistant":
			if cellTruthy(r.finishReason) {
				m.FinishReason = nullCell(r.finishReason)
			}
			if cellTruthy(r.reasoning) {
				m.Reasoning = nullCell(r.reasoning)
			}
			m.ReasoningContent = nullCell(r.reasoningContent)
			for _, f := range []struct {
				dst  *json.RawMessage
				cell any
				name string
			}{{&m.ReasoningDetails, r.reasoningDetails, "reasoning_details"}, {&m.CodexReasoningItems, r.codexReasoningItems, "codex_reasoning_items"},
				{&m.CodexMessageItems, r.codexMessageItems, "codex_message_items"}} {
				if cellTruthy(f.cell) {
					*f.dst = jsonCell(f.cell, "Failed to deserialize "+f.name+", falling back to None")
				}
			}
			if jsonTruthy(m.ToolCalls) {
				unindexedOwners = append(unindexedOwners, m)
			}
		case "user":
			clear(toolIndex)
			unindexedOwners = nil
		case "tool":
			if !cellTruthy(r.toolCallUID) && cellTruthy(r.toolCallID) {
				for _, owner := range unindexedOwners {
					indexToolCallUIDs(toolIndex, owner.ToolCalls, owner.ToolCallUIDs)
				}
				unindexedOwners = nil
				if uid := resolveToolCallUID(toolIndex, textCell(r.toolCallID)); uid != "" {
					m.ToolCallUID = uid
				}
			}
		}
		if o.includeAncestors {
			skip, cloneKey := dedupeReplayedUser(&messages, m, exactClones)
			if skip {
				continue
			}
			if cloneKey != "" {
				exactClones[cloneKey] = m
			}
		}
		messages = append(messages, m)
	}
	messages = stripReviewHarness(messages)
	stripStaleToolCallMarkers(messages)
	if o.repairAlternation && len(messages) > 0 {
		repair := s.tables.state.options.Transcript.RepairSequence
		if repair == nil {
			return nil, ErrNoSequenceRepair
		}
		repaired, count := repair(messages)
		messages = repaired
		if count > 0 {
			slog.Info(fmt.Sprintf("Repaired %d message-alternation violation(s) while "+
				"restoring session %s — durable transcript kept them, "+
				"see repair_message_sequence", count, o.sessionID))
		}
	}
	return messages, nil
}

// Restores a replayed row's data use requirement from its durable display metadata copy
func liftDataUse(m *Message) {
	members, ok := objectMembers(m.DisplayMetadata)
	if !ok {
		return
	}
	raw, _ := memberValue(members, DataUseMetadataKey)
	if !jsonTruthy(raw) {
		return
	}
	var requires []string
	if json.Unmarshal(raw, &requires) == nil {
		m.DataUse = requires
	}
}

// Ancestor lineage dedupe of one decoded user message, reporting whether to skip it and its exact clone key
func dedupeReplayedUser(messages *[]*Message, m *Message, exactClones map[string]*Message) (bool, string) {
	canonical, _ := canonicalReplayedUserContent(m)
	cloneKey := ""
	if key := contentCompareKey(canonical); m.Timestamp.Valid && key != "" && m.Role == "user" {
		cloneKey = shortestFloatText(m.Timestamp.V) + "\x00" + key
	}
	index, preferCurrent, found := 0, false, false
	if previous := exactClones[cloneKey]; cloneKey != "" && previous != nil {
		for i, candidate := range *messages {
			if candidate == previous {
				index, preferCurrent, found = i, true, true
				break
			}
		}
	}
	if !found {
		index, preferCurrent, found = findDuplicateReplayedUser(*messages, m)
	}
	if !found {
		return false, cloneKey
	}
	if preferCurrent {
		*messages = append((*messages)[:index], (*messages)[index+1:]...)
	}
	return !preferCurrent, cloneKey
}

// Model history and display history of a resume from one read, the tip's live rows and the deduped lineage
func (s *Store) GetResumeConversations(ctx context.Context, sessionID string) ([]*Message, []*Message, error) {
	ids, err := s.ResumeLineageIDs(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.fetchConversationRows(ctx, ids, "", true, true)
	if err != nil {
		return nil, nil, err
	}
	var tip []*msgRow
	for _, r := range rows {
		if r.session() == sessionID && cellTruthy(r.active) && r.roleText() != "session_meta" {
			tip = append(tip, r)
		}
	}
	model, err := s.rowsToConversation(tip, conversionOptions{sessionID: sessionID, repairAlternation: true, includeRowIDs: true, includeSummaryMarkers: true})
	if err != nil {
		return nil, nil, err
	}
	display, err := s.rowsToConversation(dedupeDisplayGenerations(rows, true), conversionOptions{sessionID: sessionID, includeAncestors: true, includeRowIDs: true})
	return model, display, err
}

// Session ids and row filter a resume count covers, the tip's live rows or the whole lineage display set
func (s *Store) resumeCountScope(ctx context.Context, sessionID string, tipOnly bool) ([]string, string, error) {
	if tipOnly {
		return []string{sessionID}, "active = 1", nil
	}
	ids, err := s.ResumeLineageIDs(ctx, sessionID)
	return ids, "(active = 1 OR compacted = 1)", err
}

// Rows a resume of the session would materialize
func (s *Store) GetResumeMessageCount(ctx context.Context, sessionID string, tipOnly bool) (int64, error) {
	ids, clause, err := s.resumeCountScope(ctx, sessionID, tipOnly)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE session_id IN ("+bindMarks(len(ids))+") AND "+clause, stringArgs(ids)...).Scan(&n)
	})
	return n, err
}

// Rows a resume materializes, refusing past limit, zero disabling the guard without counting
func (s *Store) AssertResumeSafe(ctx context.Context, sessionID string, limit int, tipOnly bool) (int, error) {
	if limit < 0 {
		return 0, ErrNegativeLimit
	}
	if limit == 0 {
		return 0, nil
	}
	ids, clause, err := s.resumeCountScope(ctx, sessionID, tipOnly)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.Read(ctx, func(c Conn) error {
		return c.QueryRowContext(ctx, "SELECT COUNT(*) FROM ("+
			"SELECT 1 FROM messages WHERE session_id IN ("+bindMarks(len(ids))+") "+
			"AND "+clause+" LIMIT ?)", append(stringArgs(ids), limit+1)...).Scan(&n)
	})
	if err != nil {
		return 0, err
	}
	if n > limit {
		scope := "across its lineage"
		if tipOnly {
			scope = "in its tip segment"
		}
		return n, &ResumeTooLargeError{MessageCount: n, Limit: limit, Scope: scope}
	}
	return n, nil
}

// Ancestor only display messages of a lineage a resume prepends, identified by row origin
func (s *Store) GetAncestorDisplayPrefix(ctx context.Context, sessionID string) ([]*Message, error) {
	ids, err := s.ResumeLineageIDs(ctx, sessionID)
	if err != nil || len(ids) <= 1 {
		return nil, err
	}
	fetched, err := s.fetchConversationRows(ctx, ids, "", true, true)
	if err != nil {
		return nil, err
	}
	rows := dedupeDisplayGenerations(fetched, true)
	ancestors := map[int64]bool{}
	for _, r := range rows {
		if r.session() != sessionID {
			ancestors[r.id] = true
		}
	}
	if len(ancestors) == 0 {
		return nil, nil
	}
	lineage, err := s.rowsToConversation(rows, conversionOptions{sessionID: sessionID, includeAncestors: true, includeRowIDs: true})
	if err != nil {
		return nil, err
	}
	var out []*Message
	for _, m := range lineage {
		if ancestors[m.RowID] {
			m.RowID = 0
			out = append(out, m)
		}
	}
	return out, nil
}

// Root id of the lineage across compression segments and delegate subagents, the session itself without a recorded parent
func (s *Store) GetConversationRoot(ctx context.Context, sessionID string) (string, error) {
	chain, err := s.sessionLineageRootToTip(ctx, sessionID)
	if err != nil || len(chain) == 0 || chain[0] == "" {
		return sessionID, err
	}
	return chain[0], nil
}

// Ordered ids of the active rows, for rewind compare and swap checks
func (s *Store) GetActiveMessageIDs(ctx context.Context, sessionID string) ([]int64, error) {
	var ids []int64
	err := s.Read(ctx, func(c Conn) error {
		var err error
		ids, err = queryInt64s(ctx, c, activeIDsSQL, sessionID)
		return err
	})
	return ids, err
}

// One integer of a single row read
func (s *Store) readInt(ctx context.Context, query string, args ...any) (int64, bool, error) {
	var v sql.NullInt64
	found := false
	err := s.Read(ctx, func(c Conn) error {
		err := c.QueryRowContext(ctx, query, args...).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
			return nil
		}
		found = err == nil
		return err
	})
	return v.Int64, found, err
}

// One text value of a single row read
func (s *Store) readText(ctx context.Context, query string, args ...any) (string, bool, error) {
	var v sql.NullString
	found := false
	err := s.Read(ctx, func(c Conn) error {
		err := c.QueryRowContext(ctx, query, args...).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
			return nil
		}
		found = err == nil
		return err
	})
	return v.String, found, err
}

// Count of messages, of one session when an id is given
func (s *Store) MessageCount(ctx context.Context, sessionID string) (int64, error) {
	query, args := "SELECT COUNT(*) FROM messages", []any{}
	if sessionID != "" {
		query, args = query+" WHERE session_id = ?", []any{sessionID}
	}
	n, _, err := s.readInt(ctx, query, args...)
	return n, err
}

// Rows a display read of this segment paints, one per display slot
func (s *Store) DisplayMessageCount(ctx context.Context, sessionID string) (int64, error) {
	n, _, err := s.readInt(ctx, "SELECT COUNT(*) FROM (SELECT DISTINCT display_order FROM messages WHERE session_id = ?"+
		displayActiveClause+displayVisibleSQL+notALaterToolCopySQL("messages")+")", sessionID)
	return n, err
}

// Reports whether the session holds an accepted input the gateway owner marked
func (s *Store) HasGatewayInputOwner(ctx context.Context, sessionID, owner string) (bool, error) {
	_, found, err := s.readInt(ctx, "SELECT 1 FROM messages WHERE session_id = ? AND role = 'user' "+
		"AND observed = 0 AND (active = 1 OR compacted = 1) "+
		"AND CASE WHEN json_valid(display_metadata) "+
		"THEN json_extract(display_metadata, '$.gateway_input_owner') END = ? LIMIT 1", sessionID, owner)
	return found, err
}

// Reports whether a platform message id is already stored in the session
func (s *Store) HasPlatformMessageID(ctx context.Context, sessionID, platformMessageID string) (bool, error) {
	_, found, err := s.readInt(ctx, "SELECT 1 FROM messages WHERE session_id = ? AND platform_message_id = ? LIMIT 1", sessionID, platformMessageID)
	return found, err
}

// Reports whether the session has any soft archived rows
func (s *Store) HasArchivedMessages(ctx context.Context, sessionID string) (bool, error) {
	_, found, err := s.readInt(ctx, "SELECT 1 FROM messages WHERE session_id = ? AND active = 0 LIMIT 1", sessionID)
	return found, err
}

// Highest id among the active rows, zero when there are none, captured when a compression starts
func (s *Store) GetActiveMessageWatermark(ctx context.Context, sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	n, _, err := s.readInt(ctx, "SELECT COALESCE(MAX(id), 0) FROM messages WHERE session_id = ? AND active = 1", sessionID)
	return n, err
}

// Conversation boundaries the routing peer has crossed, false when it never reset
func (s *Store) LatestConversationBoundary(ctx context.Context, sessionKey, source string) (int64, bool, error) {
	if sessionKey == "" || source == "" {
		return 0, false, nil
	}
	n, _, err := s.readInt(ctx, "SELECT generation FROM conversation_generations WHERE source = ? AND session_key = ?", source, sessionKey)
	return n, n > 0, err
}

// Row id of the most recent active message of a role, offset stepping back, false when none qualifies
func (s *Store) LatestMessageRowID(ctx context.Context, sessionID, role string, offset int, requireText bool) (int64, bool, error) {
	if sessionID == "" || (role != "user" && role != "assistant") || offset < 0 {
		return 0, false, nil
	}
	filter := ""
	if requireText {
		filter = "AND content IS NOT NULL AND TRIM(content) != '' "
	}
	return s.readInt(ctx, "SELECT id FROM messages WHERE session_id = ? AND role = ? "+
		"AND active = 1 "+filter+"ORDER BY id DESC LIMIT 1 OFFSET ?", sessionID, role, offset)
}

// Role of the newest active user, assistant or tool row, false when the session has none
func (s *Store) LatestConversationRole(ctx context.Context, sessionID string) (string, bool, error) {
	return s.readText(ctx, "SELECT role FROM messages WHERE session_id = ? AND active = 1 "+
		"AND role NOT IN ('session_meta', 'system') ORDER BY id DESC LIMIT 1", sessionID)
}

// Role of the active message at a row id of the session, false when there is none
func (s *Store) GetMessageRole(ctx context.Context, sessionID string, rowID int64) (string, bool, error) {
	if sessionID == "" {
		return "", false, nil
	}
	return s.readText(ctx, "SELECT role FROM messages WHERE id = ? AND session_id = ? AND active = 1", rowID, sessionID)
}
