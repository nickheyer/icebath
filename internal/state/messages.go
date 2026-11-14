package state

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/blake2b"
)

// Display metadata keys the store reads, reactions, the model-only flag and the connector data use requirement
const (
	ReactionsMetadataKey        = "reactions"
	ModelOnlyDisplayMetadataKey = "model_only"
	DataUseMetadataKey          = "data_use"
)

// One insert shape every message writer shares
const insertMessageSQL = `INSERT INTO messages (session_id, role, content, tool_call_id,
                   tool_calls, tool_name, effect_disposition, timestamp, token_count, finish_reason,
                   reasoning, reasoning_content, reasoning_details, codex_reasoning_items,
                   codex_message_items, platform_message_id, observed, _compressed_summary, active, api_content, display_kind,
                   display_metadata, display_identity, message_uid, absorbed_message_uids, tool_call_uids,
                   tool_call_uid)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// Columns the insert writes, in bind order
var messageWriteColumns = []string{"session_id", "role", "content", "tool_call_id", "tool_calls", "tool_name", "effect_disposition",
	"timestamp", "token_count", "finish_reason", "reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items",
	"codex_message_items", "platform_message_id", "observed", "_compressed_summary", "active", "api_content", "display_kind",
	"display_metadata", "display_identity", "message_uid", "absorbed_message_uids", "tool_call_uids", "tool_call_uid"}

// Payload columns a row addressed rewrite may change
var repairColumns = []string{"content", "tool_call_id", "tool_calls", "tool_name", "effect_disposition", "token_count", "finish_reason",
	"reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items", "platform_message_id",
	"observed", "_compressed_summary", "api_content", "display_kind", "display_metadata", "absorbed_message_uids", "tool_call_uids"}

// Payload columns that make up the ownership version of a row
var ownedColumns = []string{"content", "tool_call_id", "tool_calls", "tool_name", "effect_disposition", "token_count", "finish_reason",
	"reasoning", "reasoning_content", "reasoning_details", "codex_message_items", "observed", "_compressed_summary",
	"absorbed_message_uids", "tool_call_uids"}

// Columns a replay of the row sends
var replayColumns = append(append([]string{}, ownedColumns...), "api_content", "codex_reasoning_items")

// Replayed reasoning carriers among the columns
var replaySidecars = []string{"reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items"}

// Statements shared by the transcript writers
const (
	turnLeaseRowSQL           = "SELECT holder, expires_at FROM session_turn_leases WHERE conversation_id = ?"
	compressionLockRowSQL     = "SELECT holder, expires_at FROM compression_locks WHERE session_id = ?"
	endedRowSQL               = "SELECT ended_at, end_reason FROM sessions WHERE id = ?"
	deleteCompressionLockSQL  = "DELETE FROM compression_locks WHERE session_id = ? AND holder = ?"
	displayActiveClause       = " AND (active = 1 OR compacted = 1)"
	activeIDsSQL              = "SELECT id FROM messages WHERE session_id = ? AND active = 1 ORDER BY id"
	liveIdentitySQL           = "SELECT id, role, content, tool_call_id, tool_calls, message_uid FROM messages WHERE session_id = ? AND active = 1 ORDER BY id LIMIT ?"
	setCountersSQL            = "UPDATE sessions SET message_count = ?, tool_call_count = ?"
	resetCountersSQL          = "UPDATE sessions SET message_count = 0, tool_call_count = 0 WHERE id = ?"
	setDisplayMetaSQL         = "UPDATE messages SET display_metadata = ? WHERE id = ?"
	archiveActiveSQL          = "UPDATE messages SET active = 0, compacted = 1 WHERE session_id = ? AND active = 1"
	shadowedCheckpointRowsSQL = "SELECT id, codex_reasoning_items FROM messages WHERE session_id = ? AND active = 1 " +
		`AND role = 'assistant' AND id < ? AND codex_reasoning_items LIKE '%"compaction"%'`
	setCodexReasoningSQL = "UPDATE messages SET codex_reasoning_items = ? WHERE id = ?"
)

// Filter keeping model-only rows out of every display projection
var displayVisibleSQL = " AND COALESCE(" + JSONExtractSQL("display_metadata", "$."+ModelOnlyDisplayMetadataKey) + ", 0) = 0"

// Probe for a display row still missing either half of its display index
const displayIndexMissingSQL = "SELECT 1 FROM messages WHERE session_id = ?" + displayActiveClause +
	" AND (display_order IS NULL OR display_identity IS NULL) LIMIT 1"

// One durable row an alternation repair retired behind a held message that carries no row id
type RetiredRow struct {
	Role        string
	Content     Content
	ToolCallID  string
	ToolCallIDs []string
	// The message's own row, recorded before a fold rewrote its text
	Own bool
}

// Durable version another writer committed, handed back on a message whose rewrite lost the compare and swap
type CanonicalRow struct {
	// Only the presentation metadata the live message lacked
	MetadataOnly bool
	// Only the content of an assistant row another writer already filled
	ContentOnly bool
	Message     Message
}

// One transcript message as a caller keeps it live and as a conversation read returns it
type Message struct {
	// Role, an empty one stored as unknown
	Role    string
	Content Content
	// Provider tool call id of a tool result row
	ToolCallID sql.Null[string]
	// Tool calls as the raw JSON array the provider protocol carries, nil when the message makes none
	ToolCalls         json.RawMessage
	ToolName          sql.Null[string]
	EffectDisposition sql.Null[string]
	// Epoch seconds, stamped by the store when absent
	Timestamp        sql.Null[float64]
	TokenCount       sql.Null[int64]
	FinishReason     sql.Null[string]
	Reasoning        sql.Null[string]
	ReasoningContent sql.Null[string]
	// Structured reasoning carriers as raw JSON, stored on assistant rows only
	ReasoningDetails    json.RawMessage
	CodexReasoningItems json.RawMessage
	CodexMessageItems   json.RawMessage
	// The platform's own id of the message
	PlatformMessageID sql.Null[string]
	Observed          bool
	CompressedSummary bool
	// Exact string sent to the API when it differed from the content
	APIContent  sql.Null[string]
	DisplayKind sql.Null[string]
	// Presentation metadata as one raw JSON object
	DisplayMetadata json.RawMessage
	// Durable identity of the logical message, minted at its first insert
	UID string
	// Identities of the messages a merge folded into this one
	AbsorbedUIDs []string
	// Stored identity of each tool call the message makes, by provider call id
	ToolCallUIDs ToolCallUIDs
	// Stored identity of the call a tool result answers
	ToolCallUID string
	// Connector data use requirement, persisted inside the display metadata
	DataUse []string
	// Row the message was written to or read from, zero when unknown
	RowID int64
	// Message content is durable as written
	Persisted bool
	// Version digest of the stored row the message was last synced with
	Snapshot string
	// Durable version to adopt after a flush, nil when the live message stands
	Canonical *CanonicalRow
	// The flush saw this identity durable in the session before this write
	RestoredFromStore bool
	// The message must be inserted even when its identity is already durable
	PersistAfterAdmissionInterrupt bool
	// Durable user rows an alternation repair folded into this message, zero for none
	MergedDurableRows int
	// Further durable rows this message stands for on a reload without row ids
	UnnamedDurableRows int
	// Row ids a repair merged into this message
	AbsorbedRowIDs []int64
	// What names the unnamed rows at commit, one entry per counted row
	RetiredDurableRows []RetiredRow
}

// Role stored for the message, unknown when it names none
func (m *Message) role() string {
	if m.Role == "" {
		return "unknown"
	}
	return m.Role
}

// Fresh durable id of a message or a tool call occurrence
func mintUID() (string, error) {
	u, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(u[:]), nil
}

// Mints the message identity when the message carries none
func stampMessageUID(m *Message) error {
	if m.UID != "" {
		return nil
	}
	uid, err := mintUID()
	if err != nil {
		return err
	}
	m.UID = uid
	return nil
}

// Raw cells of one messages row, or the values a writer binds for one
type msgRow struct {
	id                  int64
	sessionID           any
	role                any
	content             any
	toolCallID          any
	toolCalls           any
	toolName            any
	effectDisposition   any
	timestamp           any
	tokenCount          any
	finishReason        any
	reasoning           any
	reasoningContent    any
	reasoningDetails    any
	codexReasoningItems any
	codexMessageItems   any
	platformMessageID   any
	observed            any
	compressedSummary   any
	active              any
	compacted           any
	apiContent          any
	displayKind         any
	displayMetadata     any
	displayIdentity     any
	displayOrder        any
	messageUID          any
	absorbedMessageUIDs any
	toolCallUIDs        any
	toolCallUID         any
	// Columns the query selected
	has map[string]bool
}

// Field holding one column, nil for a column the row does not carry
func (r *msgRow) field(col string) *any {
	switch col {
	case "session_id":
		return &r.sessionID
	case "role":
		return &r.role
	case "content":
		return &r.content
	case "tool_call_id":
		return &r.toolCallID
	case "tool_calls":
		return &r.toolCalls
	case "tool_name":
		return &r.toolName
	case "effect_disposition":
		return &r.effectDisposition
	case "timestamp":
		return &r.timestamp
	case "token_count":
		return &r.tokenCount
	case "finish_reason":
		return &r.finishReason
	case "reasoning":
		return &r.reasoning
	case "reasoning_content":
		return &r.reasoningContent
	case "reasoning_details":
		return &r.reasoningDetails
	case "codex_reasoning_items":
		return &r.codexReasoningItems
	case "codex_message_items":
		return &r.codexMessageItems
	case "platform_message_id":
		return &r.platformMessageID
	case "observed":
		return &r.observed
	case "_compressed_summary":
		return &r.compressedSummary
	case "active":
		return &r.active
	case "compacted":
		return &r.compacted
	case "api_content":
		return &r.apiContent
	case "display_kind":
		return &r.displayKind
	case "display_metadata":
		return &r.displayMetadata
	case "display_identity":
		return &r.displayIdentity
	case "display_order":
		return &r.displayOrder
	case "message_uid":
		return &r.messageUID
	case "absorbed_message_uids":
		return &r.absorbedMessageUIDs
	case "tool_call_uids":
		return &r.toolCallUIDs
	case "tool_call_uid":
		return &r.toolCallUID
	}
	return nil
}

// Cell of one column, nil when the row does not carry it
func (r *msgRow) col(name string) any {
	if f := r.field(name); f != nil {
		return *f
	}
	return nil
}

// Role text of the row
func (r *msgRow) roleText() string { return textCell(r.role) }

// Session id text of the row
func (r *msgRow) session() string { return textCell(r.sessionID) }

// Text of a TEXT cell, empty for anything else
func textCell(v any) string {
	s, _ := v.(string)
	return s
}

// Reports whether a cell passes a truth test, NULL, zero and empty text failing it
func cellTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case []byte:
		return len(t) > 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case bool:
		return t
	}
	return true
}

// Float of a numeric cell, false for anything else
func floatCell(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// Every message row of a query as raw cells
func queryMsgRows(rows *sql.Rows, err error) ([]*msgRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []*msgRow
	for rows.Next() {
		r := &msgRow{has: make(map[string]bool, len(cols))}
		args := make([]any, len(cols))
		var id any
		for i, col := range cols {
			r.has[col] = true
			switch f := r.field(col); {
			case col == "id":
				args[i] = &id
			case f != nil:
				args[i] = f
			default:
				args[i] = new(any)
			}
		}
		if err := rows.Scan(args...); err != nil {
			return nil, err
		}
		r.id, _ = cellInt(id)
		out = append(out, r)
	}
	return out, rows.Err()
}

// First message row of a query, nil when it returned none
func queryMsgRow(rows *sql.Rows, err error) (*msgRow, error) {
	all, err := queryMsgRows(rows, err)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[0], nil
}

// Values bound for the insert, in column order
func (r *msgRow) insertArgs() []any {
	args := make([]any, len(messageWriteColumns))
	for i, col := range messageWriteColumns {
		args[i] = r.col(col)
	}
	return args
}

// Version digest of the owned columns of a stored row
func rowSnapshot(r *msgRow) string {
	digest, err := blake2b.New(16, nil)
	if err != nil {
		return ""
	}
	for _, col := range ownedColumns {
		var tag byte
		var data []byte
		switch v := r.col(col).(type) {
		case nil:
			digest.Write([]byte("N"))
			continue
		case []byte:
			tag, data = 'B', v
		case string:
			tag, data = 'S', []byte(v)
		case float64:
			tag, data = 'F', []byte(shortestFloatText(v))
		case int64:
			tag, data = 'I', []byte(strconv.FormatInt(v, 10))
		case int:
			tag, data = 'I', []byte(strconv.Itoa(v))
		default:
			tag, data = 'S', []byte(fmt.Sprint(v))
		}
		var size [8]byte
		for i, n := 7, uint64(len(data)); i >= 0; i, n = i-1, n>>8 {
			size[i] = byte(n)
		}
		digest.Write(append(append([]byte{tag}, size[:]...), data...))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// Call id, id and response item id strings of one tool call entry
func toolCallIDFields(raw json.RawMessage) (callID, id, responseItemID string) {
	members, ok := objectMembers(raw)
	if !ok {
		return "", "", ""
	}
	text := func(key string) string {
		v, _ := memberValue(members, key)
		if len(v) == 0 || v[0] != '"' {
			return ""
		}
		return jsonString(v)
	}
	return text("call_id"), text("id"), text("response_item_id")
}

// Every wire spelling of the given identifiers, each whole value before its pipe separated parts
func expandToolIDVariants(values ...string) []string {
	var out []string
	add := func(v string) {
		if v != "" && !containsString(out, v) {
			out = append(out, v)
		}
	}
	for _, raw := range values {
		value := strings.TrimFunc(raw, previewSpace)
		if value == "" {
			continue
		}
		add(value)
		for _, part := range strings.Split(value, "|") {
			add(strings.TrimFunc(part, previewSpace))
		}
	}
	return out
}

// Tool calls of a message as its array elements, nil when it carries no list
func toolCallElements(raw json.RawMessage) []json.RawMessage {
	elements, _ := arrayElements(raw)
	return elements
}

// Tool calls value a writer serializes, a value that is not JSON reading as an empty list
func parseToolCalls(raw json.RawMessage) json.RawMessage {
	t := json.RawMessage(strings.TrimSpace(string(raw)))
	if len(t) == 0 {
		return nil
	}
	if !json.Valid(t) {
		return json.RawMessage("[]")
	}
	if t[0] == '"' {
		var inner string
		if json.Unmarshal(t, &inner) != nil || !json.Valid([]byte(inner)) {
			return json.RawMessage("[]")
		}
		return json.RawMessage(strings.TrimSpace(inner))
	}
	return t
}

// Tool calls one inserted message counts for, a list by its length and any other value as one
func toolCallsCount(parsed json.RawMessage) int {
	if len(parsed) == 0 || string(parsed) == "null" {
		return 0
	}
	if elements, ok := arrayElements(parsed); ok {
		return len(elements)
	}
	return 1
}

// Count of a stored tool calls cell, a list by its length, scalar for a truthy value that is no list
func toolCallsLen(cell any, scalar int) int {
	text, ok := cell.(string)
	if !ok {
		if cellTruthy(cell) {
			return scalar
		}
		return 0
	}
	parsed := parseStoredJSON(text)
	if elements, ok := arrayElements(parsed); ok {
		return len(elements)
	}
	if jsonTruthy(parsed) {
		return scalar
	}
	return 0
}

// Stored JSON text as a value, text that is not JSON reading as an empty list
func parseStoredJSON(text string) json.RawMessage {
	t := strings.TrimSpace(text)
	if !json.Valid([]byte(t)) {
		return json.RawMessage("[]")
	}
	return json.RawMessage(t)
}

// Registers an assistant message's tool call identities under every pairing id variant of its calls
func indexToolCallUIDs(index map[string]string, toolCalls json.RawMessage, uids ToolCallUIDs) {
	type call struct {
		id       string
		variants []string
	}
	var calls []call
	for _, raw := range toolCallElements(toolCalls) {
		callID, id, item := toolCallIDFields(raw)
		calls = append(calls, call{CoalesceToolCallID(callID, id), expandToolIDVariants(callID, id, item)})
	}
	for _, c := range calls {
		for _, v := range c.variants {
			delete(index, v)
		}
	}
	if len(uids) == 0 {
		return
	}
	occurrence := map[string]int{}
	for _, c := range calls {
		stored, ok := uids[c.id]
		if !ok {
			continue
		}
		uid := stored.One
		if stored.Many != nil {
			nth := occurrence[c.id]
			occurrence[c.id] = nth + 1
			uid = ""
			if nth < len(stored.Many) {
				uid = stored.Many[nth]
			}
		}
		if uid == "" {
			continue
		}
		for _, v := range c.variants {
			index[v] = uid
		}
	}
}

// Identity an indexed assistant message minted for a tool call id in any spelling, empty when none
func resolveToolCallUID(index map[string]string, toolCallID string) string {
	if len(index) == 0 || toolCallID == "" {
		return ""
	}
	for _, v := range expandToolIDVariants(toolCallID) {
		if uid := index[v]; uid != "" {
			return uid
		}
	}
	return ""
}

// Gives every tool call of an assistant message an identity and pairs a tool result with the call the batch named
func stampToolCallUIDs(m *Message, toolCalls json.RawMessage, batchIndex map[string]string) error {
	switch m.Role {
	case "assistant":
		if !jsonTruthy(toolCalls) {
			return nil
		}
		uids := ToolCallUIDs{}
		for k, v := range m.ToolCallUIDs {
			if k != "" && (v.Many != nil || v.One != "") {
				uids[k] = v
			}
		}
		for _, raw := range toolCallElements(toolCalls) {
			callID, id, _ := toolCallIDFields(raw)
			key := CoalesceToolCallID(callID, id)
			if key == "" {
				continue
			}
			if _, have := uids[key]; !have {
				uid, err := mintUID()
				if err != nil {
					return err
				}
				uids[key] = ToolCallUID{One: uid}
			}
		}
		if len(uids) > 0 {
			m.ToolCallUIDs = uids
		}
		indexToolCallUIDs(batchIndex, toolCalls, m.ToolCallUIDs)
	case "user":
		clear(batchIndex)
	case "tool":
		if m.ToolCallUID == "" && m.ToolCallID.Valid {
			if uid := resolveToolCallUID(batchIndex, m.ToolCallID.V); uid != "" {
				m.ToolCallUID = uid
			}
		}
	}
	return nil
}

// Stored timestamp of a message, fallback when it carries none or one outside the trusted epoch window
func coerceTimestamp(ts sql.Null[float64], fallback float64) float64 {
	if !ts.Valid {
		return fallback
	}
	if v, ok := trustedEpoch(ts.V, "message timestamp"); ok {
		if v == 0 {
			return 0
		}
		return v
	}
	return fallback
}

// Epoch seconds inside the trusted window, false after a warning for anything outside it
func trustedEpoch(v float64, field string) (float64, bool) {
	if math.IsNaN(v) || v < 0 || v > 4200000000 {
		slog.Warn(fmt.Sprintf("Ignoring corrupt %s %s", field, shortestFloatText(v)))
		return 0, false
	}
	return v, true
}

// Epoch seconds of a stored timestamp cell, false for NULL, empty text and anything untrusted
func cellEpoch(cell any, field string) (float64, bool) {
	switch t := cell.(type) {
	case nil:
		return 0, false
	case float64:
		return trustedEpoch(t, field)
	case int64:
		return trustedEpoch(float64(t), field)
	case string:
		if t == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			f = math.NaN()
		}
		return trustedEpoch(f, field)
	}
	return trustedEpoch(math.NaN(), field)
}

// Display metadata of a message with its data use requirement merged in
func (m *Message) displayMetadataWithDataUse() (json.RawMessage, error) {
	if len(m.DataUse) == 0 {
		return m.DisplayMetadata, nil
	}
	members, isObject := objectMembers(m.DisplayMetadata)
	if t := strings.TrimSpace(string(m.DisplayMetadata)); !isObject && t != "" && t != "null" {
		return m.DisplayMetadata, nil
	}
	requires, err := json.Marshal(m.DataUse)
	if err != nil {
		return nil, err
	}
	text, err := encodeConfigMembers(setConfigMember(members, DataUseMetadataKey, requires))
	return json.RawMessage(text), err
}

// Bound nullable text, NULL for an absent value
func nullText(v sql.Null[string]) any {
	if !v.Valid {
		return nil
	}
	return v.V
}

// Bound nullable text with invalid sequences replaced, NULL for an absent value
func scrubbedText(v sql.Null[string]) any {
	if !v.Valid {
		return nil
	}
	return replaceSurrogates(v.V)
}

// Values the insert binds for one message, its tool calls already parsed, reasoning kept only when asked
func (s *Store) messageRowParams(sessionID, role string, m *Message, toolCalls json.RawMessage, timestamp float64, keepReasoning bool) (*msgRow, error) {
	content, err := m.Content.encode()
	if err != nil {
		return nil, err
	}
	r := &msgRow{sessionID: sessionID, role: role, content: content, toolCallID: nullText(m.ToolCallID), timestamp: timestamp,
		toolName: scrubbedText(m.ToolName), effectDisposition: nullText(m.EffectDisposition), finishReason: nullText(m.FinishReason),
		platformMessageID: nullText(m.PlatformMessageID), observed: int64(boolInt(m.Observed)),
		compressedSummary: int64(boolInt(m.CompressedSummary)), active: int64(1), apiContent: scrubbedText(m.APIContent),
		displayKind: scrubbedText(m.DisplayKind)}
	if jsonTruthy(toolCalls) {
		text, err := spacedJSON(toolCalls)
		if err != nil {
			return nil, fmt.Errorf("tool_calls is not valid JSON: %w", err)
		}
		r.toolCalls = text
	}
	if m.TokenCount.Valid {
		r.tokenCount = m.TokenCount.V
	}
	if keepReasoning {
		r.reasoning, r.reasoningContent = scrubbedText(m.Reasoning), scrubbedText(m.ReasoningContent)
		for _, f := range []struct {
			dst *any
			raw json.RawMessage
		}{{&r.reasoningDetails, m.ReasoningDetails}, {&r.codexReasoningItems, m.CodexReasoningItems}, {&r.codexMessageItems, m.CodexMessageItems}} {
			if *f.dst, err = reasoningJSONText(f.raw); err != nil {
				return nil, err
			}
		}
	}
	meta, err := m.displayMetadataWithDataUse()
	if err != nil {
		return nil, err
	}
	if r.displayMetadata, err = encodeDisplayMetadata(meta); err != nil {
		return nil, err
	}
	r.displayIdentity = displayIdentity(displayKeyOf(&msgRow{role: role, content: content, timestamp: timestamp, toolCallID: r.toolCallID,
		toolCalls: r.toolCalls, toolName: r.toolName, displayKind: nullText(m.DisplayKind), displayMetadata: r.displayMetadata}))
	if m.UID != "" {
		r.messageUID = m.UID
	}
	if text, ok, err := EncodeAbsorbedUIDs(m.AbsorbedUIDs); err != nil {
		return nil, err
	} else if ok {
		r.absorbedMessageUIDs = text
	}
	if text, ok, err := EncodeToolCallUIDs(validToolCallUIDs(m.ToolCallUIDs)); err != nil {
		return nil, err
	} else if ok {
		r.toolCallUIDs = text
	}
	if m.ToolCallUID != "" {
		r.toolCallUID = m.ToolCallUID
	}
	return r, nil
}

// Tool call identities with empty keys, empty uids and empty lists dropped
func validToolCallUIDs(m ToolCallUIDs) ToolCallUIDs {
	out := ToolCallUIDs{}
	for k, v := range m {
		if k == "" {
			continue
		}
		if v.Many != nil {
			ok := len(v.Many) > 0
			for _, u := range v.Many {
				ok = ok && u != ""
			}
			if ok {
				out[k] = v
			}
		} else if v.One != "" {
			out[k] = v
		}
	}
	return out
}

// Durable shape of one live message exactly as an insert would bind it
func (s *Store) serializedMessageRow(sessionID string, m *Message, timestamp float64) (*msgRow, error) {
	role := m.role()
	return s.messageRowParams(sessionID, role, m, parseToolCalls(m.ToolCalls), timestamp, role == "assistant")
}

// Display key text of a row, a summary carrier keyed by the live turn it wraps
func displayKeyOf(r *msgRow) string {
	role := r.roleText()
	if ids, ok := stableToolCallIDs(role, textCell(r.toolCalls)); ok {
		return stableToolKeyText(r.timestamp, r.toolCallID, ids, r.toolName)
	}
	content := r.content
	if role == "user" {
		split := splitUserOriginatedTurn(userTurn{content: decodeContent(r.content), displayKind: textCell(r.displayKind)})
		if split.handoff && split.live {
			if enc, err := split.liveContent.encode(); err == nil {
				content = enc
			}
		}
	}
	return contentKeyText(role, content, r.timestamp, r.toolCallID, r.toolCalls, r.toolName)
}

// Reports a row the model reads but no display projection paints
func isModelOnlyRow(r *msgRow) bool {
	members, ok := objectMembers(decodeDisplayMetadata(r.displayMetadata))
	if !ok {
		return false
	}
	v, _ := memberValue(members, ModelOnlyDisplayMetadataKey)
	return jsonTruthy(v)
}

// Bumps the session counters after an insert, a single message using the literal increment
func bumpSessionCounters(ctx context.Context, c Conn, sessionID string, inserted, toolCalls int, unit bool) error {
	inc, args := "1", []any{}
	if !unit {
		inc, args = "?", []any{inserted}
	}
	var err error
	if toolCalls > 0 {
		_, err = c.ExecContext(ctx, `UPDATE sessions SET message_count = message_count + `+inc+`,
                       tool_call_count = tool_call_count + ? WHERE id = ?`, append(args, toolCalls, sessionID)...)
	} else if inserted > 0 {
		_, err = c.ExecContext(ctx, "UPDATE sessions SET message_count = message_count + "+inc+" WHERE id = ?", append(args, sessionID)...)
	}
	return err
}
