package state

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	hermesInsertMessageSQL = `INSERT INTO messages (session_id, role, content, tool_call_id,
                   tool_calls, tool_name, effect_disposition, timestamp, token_count, finish_reason,
                   reasoning, reasoning_content, reasoning_details, codex_reasoning_items,
                   codex_message_items, platform_message_id, observed, _compressed_summary, active, api_content, display_kind,
                   display_metadata, display_identity, message_uid, absorbed_message_uids, tool_call_uids,
                   tool_call_uid)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	bumpGenerationSQL = `
            INSERT INTO conversation_generations (source, session_key, generation)
            VALUES (?, ?, 1)
            ON CONFLICT(source, session_key) DO UPDATE
                SET generation = conversation_generations.generation + 1
            `
	turnLeaseRowSQL           = "SELECT holder, expires_at FROM session_turn_leases WHERE conversation_id = ?"
	deleteCompressionLockSQL  = "DELETE FROM compression_locks WHERE session_id = ? AND holder = ?"
	displayActiveClause       = " AND (active = 1 OR compacted = 1)"
	displayMetaRowSQL         = "SELECT display_metadata FROM messages WHERE id = ? AND session_id IN ({ids})" + displayActiveClause
	activeIDsSQL              = "SELECT id FROM messages WHERE session_id = ? AND active = 1 ORDER BY id"
	resetCountersSQL          = "UPDATE sessions SET message_count = 0, tool_call_count = 0 WHERE id = ?"
	setDisplayMetaSQL         = "UPDATE messages SET display_metadata = ? WHERE id = ?"
	shadowedCheckpointRowsSQL = "SELECT id, codex_reasoning_items FROM messages WHERE session_id = ? AND active = 1 AND role = 'assistant' AND id < ? AND codex_reasoning_items LIKE '%\"compaction\"%'"
	setCodexReasoningSQL      = "UPDATE messages SET codex_reasoning_items = ? WHERE id = ?"
	ReactionsMetadataKey      = "reactions"
)

// Message dict fields append_message accepts, every other key of the caller dict is ignored
var appendMessageFields = []string{"role", "content", "tool_name", "tool_calls", "tool_call_id", "token_count",
	"finish_reason", "reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items",
	"codex_message_items", "platform_message_id", "observed", "effect_disposition", "_compressed_summary",
	"timestamp", "api_content", "display_kind", "display_metadata", "message_uid"}

// Explicit message timestamp inside the sane epoch window, the default otherwise, negative zero stored as zero
func coerceMessageTimestamp(value any, def float64) float64 {
	result, ok := pydatetime.CoerceEpoch(value, "", "message timestamp")
	if !ok {
		return def
	}
	if result == 0 {
		return 0
	}
	return result
}

// Tool calls list as given, a JSON string decoded, undecodable text an empty list
func parseToolCalls(toolCalls any) any {
	s, ok := toolCalls.(string)
	if !ok {
		return toolCalls
	}
	v, err := pyLoads(s)
	if err != nil {
		return []any{}
	}
	return v
}

// Number of tool calls a parsed value stands for
func toolCallsCount(toolCalls any) int {
	if toolCalls == nil {
		return 0
	}
	if list, ok := toolCalls.([]any); ok {
		return len(list)
	}
	return 1
}

// Count of a stored tool_calls column, scalar for a truthy non list
func toolCallsLen(raw any, scalar int) int {
	parsed := parseToolCalls(raw)
	if list, ok := parsed.([]any); ok {
		return len(list)
	}
	if pyTruthy(parsed) {
		return scalar
	}
	return 0
}

// String values with lone surrogates scrubbed, other values unchanged
func scrubSurrogates(value any) any {
	if s, ok := value.(string); ok {
		return SanitizeSurrogates(s)
	}
	return value
}

// Lock or lease row whose holder expired or is a provably dead local process
func staleHolder(row map[string]any, now float64) bool {
	expires, _ := pyFloatOf(row["expires_at"])
	holder, _ := row["holder"].(string)
	return expires <= now || compressionLockHolderProcessIsDead(holder)
}

// One row of a statement on the caller connection as a plain map, nil when no row matches
func connRow(ctx context.Context, conn SQLiteConn, query string, args ...any) (map[string]any, error) {
	rows, err := queryObjects(ctx, conn, query, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return objMap(rows[0]), nil
}

// Plain map view of a row object
func objMap(row *pyjson.Object) map[string]any {
	if row == nil {
		return nil
	}
	out := make(map[string]any, len(row.Keys))
	for _, k := range row.Keys {
		out[k] = row.Vals[k]
	}
	return out
}

// Advances the peer conversation generation past a reset boundary in the writing transaction
func bumpConversationGeneration(ctx context.Context, conn SQLiteConn, sessionID, endReason string) error {
	if !containsStr(resetEndReasons, endReason) {
		return nil
	}
	row, err := connRow(ctx, conn, "SELECT source, session_key FROM sessions WHERE id = ?", sessionID)
	if err != nil || row == nil {
		return err
	}
	source, sessionKey := pyStrip(rowStrOrMap(row, "source")), pyStrip(rowStrOrMap(row, "session_key"))
	if source != "" && sessionKey != "" {
		_, err = conn.ExecContext(ctx, bumpGenerationSQL, source, sessionKey)
	}
	return err
}

// Python str of a map value after the or empty fallback
func rowStrOrMap(row map[string]any, key string) string {
	if !pyTruthy(row[key]) {
		return ""
	}
	return pyStr(row[key])
}

// Whether a string list holds the value
func containsStr(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// Structured content as a prefixed JSON string, text scrubbed of lone surrogates and a literal prefix escaped
func encodeMessageContent(content any) any {
	switch c := content.(type) {
	case string:
		c = SanitizeSurrogates(c)
		if !strings.HasPrefix(c, contentJSONPrefix) {
			return c
		}
		content = c
	case nil, []byte, int, int64, float64, bool:
		return content
	}
	text, err := pyDumps(content, pyJSONDefault)
	if err != nil {
		return SanitizeSurrogates(pyStr(content))
	}
	return contentJSONPrefix + text
}

// Display metadata serialized for its TEXT column, serialized JSON objects kept and anything else dropped
func encodeDisplayMetadata(displayMetadata any) (any, error) {
	if !pyTruthy(displayMetadata) {
		return nil, nil
	}
	if s, ok := displayMetadata.(string); ok {
		v, err := jsonOr(s, jsonInvalid, "Ignoring non-JSON display metadata on write")
		if err != nil {
			return nil, err
		}
		if v == jsonInvalid {
			return nil, nil
		}
		if !isMapping(v) {
			slog.Warn("Ignoring non-object display metadata on write")
			return nil, nil
		}
		displayMetadata = v
	} else if !isMapping(displayMetadata) {
		slog.Warn(fmt.Sprintf("Ignoring unexpected display metadata type on write: %s", pyrepr.TypeName(displayMetadata)))
		return nil, nil
	}
	text, err := pyDumps(displayMetadata, pyJSONDefault)
	if err != nil {
		return nil, err
	}
	return text, nil
}

// Structured reasoning field for its TEXT column, strings stored as given
func reasoningJSONText(value any) (any, error) {
	if !pyTruthy(value) {
		return nil, nil
	}
	if s, ok := value.(string); ok {
		return s, nil
	}
	text, err := pyDumps(value, pyJSONDefault)
	if err != nil {
		return nil, err
	}
	return text, nil
}

// Historical display key repr, a user handoff carrier keyed by its normalized live content
func displayDedupeKey(row map[string]any) (string, error) {
	dedupeContent := row["content"]
	if row["role"] == "user" {
		handoff, live := SplitUserOriginatedTurn(map[string]any{"role": "user", "content": decodeMessageContent(row["content"]),
			"display_kind": row["display_kind"], "display_metadata": decodeDisplayMetadata(row["display_metadata"])})
		if handoff != nil && live != nil {
			dedupeContent = encodeMessageContent(live["content"])
		}
	}
	key, ok, err := stableToolKey(row)
	if err != nil || ok {
		return key, err
	}
	return "(" + pyReprValue(row["role"]) + ", " + pyReprValue(dedupeContent) + ", " + pyReprValue(row["timestamp"]) + ", " +
		pyReprValue(row["tool_call_id"]) + ", " + pyReprValue(row["tool_calls"]) + ", " + pyReprValue(row["tool_name"]) + ")", nil
}

// Display key hash every reader and writer calls, swappable so tests can count hashing work
var displayIdentityOf = displayIdentityDigest

// Fixed width durable identity of a display key for indexed generation lookup
func displayIdentityDigest(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// Display identity a row takes in id order, a compaction carrier folding onto the first row of its uid
func foldSummaryCarrier(row map[string]any, key string, firstByUID map[string]string) string {
	uid, _ := row["message_uid"].(string)
	if uid == "" {
		return key
	}
	if pyTruthy(row["_compressed_summary"]) {
		if first, ok := firstByUID[uid]; ok {
			return first
		}
		return key
	}
	if _, ok := firstByUID[uid]; !ok {
		firstByUID[uid] = key
	}
	return key
}

// Rows collapsed so each logical message appears once, live then newest copy preferred, ordered by first row id
func dedupeDisplayGenerations(rows []*pyjson.Object, dropRemoved bool) ([]*pyjson.Object, error) {
	seen := map[string]*pyjson.Object{}
	firstID := map[string]int64{}
	uidKey := map[string]string{}
	removed := map[string]bool{}
	identities := map[string][]string{}
	removedTS := map[string]bool{}
	laterCopies := laterToolCopyIDs(rows)
	if dropRemoved {
		for _, row := range rows {
			if !pyTruthy(rowValue(row, "active")) && !pyTruthy(rowValue(row, "compacted")) {
				removedTS[pyReprValue(rowValue(row, "timestamp"))] = true
			}
		}
		dropRemoved = len(removedTS) > 0
	}
	var order []string
	for _, obj := range rows {
		row := objMap(obj)
		var copies []string
		key := ""
		haveKey := false
		if dropRemoved {
			gone := !pyTruthy(row["active"]) && !pyTruthy(row["compacted"])
			var stored any
			if gone {
				stored = row["display_identity"]
			}
			if !pyTruthy(stored) {
				k, err := displayDedupeKey(row)
				if err != nil {
					return nil, err
				}
				key, haveKey = k, true
			}
			hashed := ""
			if pyTruthy(stored) {
				hashed = "h:" + string(identityBytes(stored))
			} else if haveKey && (gone || removedTS[pyReprValue(row["timestamp"])]) {
				hashed = "h:" + string(displayIdentityOf(key))
			}
			if hashed != "" {
				copies = append(copies, hashed)
			}
			for _, uid := range append([]any{row["message_uid"]}, anyStrings(uidList(row["absorbed_message_uids"]))...) {
				if pyTruthy(uid) {
					copies = append(copies, "uid:"+pyStr(uid))
				}
			}
			for _, c := range copies {
				removed[c] = gone
			}
			if gone {
				continue
			}
		}
		id, _ := rowInt(obj, "id")
		if isModelOnlyRow(obj) || laterCopies[id] {
			continue
		}
		if !haveKey {
			k, err := displayDedupeKey(row)
			if err != nil {
				return nil, err
			}
			key = k
		}
		key = foldSummaryCarrier(row, key, uidKey)
		cur, ok := seen[key]
		if !ok {
			order = append(order, key)
		}
		if !ok || rowNewer(obj, cur) {
			seen[key] = obj
			identities[key] = copies
		}
		if first, ok := firstID[key]; !ok || id < first {
			firstID[key] = id
		}
	}
	sortStableBy(order, func(a, b string) bool { return firstID[a] < firstID[b] })
	out := make([]*pyjson.Object, 0, len(order))
	for _, key := range order {
		if dropRemoved {
			hidden := false
			for _, c := range identities[key] {
				if removed[c] {
					hidden = true
					break
				}
			}
			if hidden {
				continue
			}
		}
		out = append(out, seen[key])
	}
	return out, nil
}

// Whether a row beats the current representative on the active flag then the id
func rowNewer(row, cur *pyjson.Object) bool {
	ra, _ := rowInt(row, "active")
	ca, _ := rowInt(cur, "active")
	if ra != ca {
		return ra > ca
	}
	ri, _ := rowInt(row, "id")
	ci, _ := rowInt(cur, "id")
	return ri > ci
}

// Raw bytes of a stored display identity cell
func identityBytes(v any) []byte {
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	}
	return []byte(pyStr(v))
}

// String list as a list of values
func anyStrings(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Stable insertion sort of keys by a comparison, keys arrive nearly sorted
func sortStableBy(keys []string, less func(a, b string) bool) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && less(keys[j], keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}

// Options of the transcript write admission checks every writer runs inside its transaction
type transcriptGuards struct {
	compressionLockHolder        *string
	turnLeaseHolder              *string
	turnLeaseTTLSeconds          *float64
	rejectActiveTurnLease        bool
	rejectActiveCompressionLock  bool
	allowClosedCompressionParent bool
}

// Transcript write admission checks run inside the write transaction, refusing lost leases and closed parents
func (d *SessionDB) checkTranscriptWriteGuards(ctx context.Context, conn SQLiteConn, sessionID string, g transcriptGuards) error {
	if g.rejectActiveCompressionLock {
		lock, err := connRow(ctx, conn, compressionLockRowSQL, sessionID)
		if err != nil {
			return err
		}
		if lock != nil {
			if staleHolder(lock, now()) {
				if _, err := conn.ExecContext(ctx, deleteCompressionLockSQL, sessionID, lock["holder"]); err != nil {
					return err
				}
			} else if g.compressionLockHolder == nil || lock["holder"] != *g.compressionLockHolder {
				return &SessionCompressionInProgressError{Msg: "Session " + pyRepr(sessionID) + " is being compressed by another writer"}
			}
		}
	}
	holder := ""
	if g.turnLeaseHolder != nil {
		holder = *g.turnLeaseHolder
	}
	if holder != "" || g.rejectActiveTurnLease {
		conversationID, err := d.sessionTurnLeaseKeyOnConn(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		lease, err := connRow(ctx, conn, turnLeaseRowSQL, conversationID)
		if err != nil {
			return err
		}
		current := now()
		if holder != "" {
			if lease == nil || lease["holder"] != holder {
				return &SessionTurnLeaseLostError{Msg: "Session turn lease lost; refusing transcript write for " + pyRepr(sessionID)}
			}
			if expires, _ := pyFloatOf(lease["expires_at"]); expires <= current {
				ttl := 300.0
				if g.turnLeaseTTLSeconds != nil {
					ttl = *g.turnLeaseTTLSeconds
				}
				if _, err := conn.ExecContext(ctx, "UPDATE session_turn_leases SET expires_at = ? WHERE conversation_id = ? AND holder = ?",
					current+max(0.1, ttl), conversationID, holder); err != nil {
					return err
				}
			}
		} else if lease != nil {
			if !staleHolder(lease, current) {
				return &SessionTurnLeaseLostError{Msg: "Session has an active turn lease; refusing transcript mutation for " + pyRepr(sessionID)}
			}
			if _, err := conn.ExecContext(ctx, "DELETE FROM session_turn_leases WHERE conversation_id = ? AND holder = ?", conversationID, lease["holder"]); err != nil {
				return err
			}
		}
	}
	ended, err := connRow(ctx, conn, endedRowSQL, sessionID)
	if err != nil {
		return err
	}
	if endedByCompression(ended) && !g.allowClosedCompressionParent {
		return &CompressionSessionClosedError{SessionID: sessionID}
	}
	return nil
}

// Conversation lease key on the caller connection through the test override when one is installed
func (d *SessionDB) sessionTurnLeaseKeyOnConn(ctx context.Context, conn SQLiteConn, sessionID string) (string, error) {
	if d.leaseSeams.keyOnConn != nil {
		return d.leaseSeams.keyOnConn(ctx, conn, sessionID)
	}
	return d.walkTurnLeaseKeyOnConn(ctx, conn, sessionID)
}

// Conversation lease key reached by walking compression parents on the caller connection
func (d *SessionDB) walkTurnLeaseKeyOnConn(ctx context.Context, conn SQLiteConn, sessionID string) (string, error) {
	if sessionID == "" {
		return sessionID, nil
	}
	load := func(sid string) (*pyjson.Object, error) {
		rows, err := queryObjects(ctx, conn, "SELECT id, parent_session_id, source, model_config, end_reason FROM sessions WHERE id = ?", sid)
		if err != nil || len(rows) == 0 {
			return nil, err
		}
		return rows[0], nil
	}
	current, err := load(sessionID)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{sessionID: true}
	for current != nil {
		parentID, _ := rowValue(current, "parent_session_id").(string)
		if parentID == "" || seen[parentID] || isExplicitForkChildRow(current, true) {
			break
		}
		parent, err := load(parentID)
		if err != nil {
			return "", err
		}
		if parent == nil || rowValue(parent, "end_reason") != "compression" {
			break
		}
		seen[parentID] = true
		current = parent
	}
	if current == nil {
		return sessionID, nil
	}
	if id := rowValue(current, "id"); pyTruthy(id) {
		return pyStr(id), nil
	}
	return sessionID, nil
}

// Bind values of the shared message INSERT for one message dict, reasoning columns NULL unless kept
func messageRowParams(sessionID, role string, msg map[string]any, toolCalls any, timestamp float64, keepReasoning bool) ([]any, error) {
	strOrNone := func(v any) any {
		if s, ok := v.(string); ok {
			return SanitizeSurrogates(s)
		}
		return nil
	}
	reasoning := func(key string) any {
		if keepReasoning {
			return msg[key]
		}
		return nil
	}
	encodedContent := encodeMessageContent(msg["content"])
	var encodedToolCalls any
	if pyTruthy(toolCalls) {
		text, err := pyDumps(toolCalls, pyJSONDefault)
		if err != nil {
			return nil, err
		}
		encodedToolCalls = text
	}
	encodedToolName := scrubSurrogates(msg["tool_name"])
	displayMetadata := msg["display_metadata"]
	if pyTruthy(msg[DataUse]) && (displayMetadata == nil || isMapping(displayMetadata)) {
		merged := pyObject{}
		if displayMetadata != nil {
			merged = toPyObject(displayMetadata)
		}
		displayMetadata = pyObjectSet(merged, DataUse, append([]any{}, pyIter(msg[DataUse])...))
	}
	encodedMeta, err := encodeDisplayMetadata(displayMetadata)
	if err != nil {
		return nil, err
	}
	key, err := displayDedupeKey(map[string]any{"role": role, "content": encodedContent, "timestamp": timestamp,
		"tool_call_id": msg["tool_call_id"], "tool_calls": encodedToolCalls, "tool_name": encodedToolName,
		"display_kind": msg["display_kind"], "display_metadata": encodedMeta})
	if err != nil {
		return nil, err
	}
	params := []any{sessionID, role, encodedContent, msg["tool_call_id"], encodedToolCalls, encodedToolName,
		msg["effect_disposition"], timestamp, msg["token_count"], msg["finish_reason"],
		scrubSurrogates(reasoning("reasoning")), scrubSurrogates(reasoning("reasoning_content"))}
	for _, k := range []string{"reasoning_details", "codex_reasoning_items", "codex_message_items"} {
		text, err := reasoningJSONText(reasoning(k))
		if err != nil {
			return nil, err
		}
		params = append(params, text)
	}
	platformID := msg["platform_message_id"]
	if !pyTruthy(platformID) {
		platformID = msg["message_id"]
	}
	params = append(params, platformID, boolInt(pyTruthy(msg["observed"])), boolInt(pyTruthy(msg["_compressed_summary"])), 1,
		strOrNone(msg["api_content"]), strOrNone(msg["display_kind"]), encodedMeta, displayIdentityOf(key))
	if uid, ok := MessageUIDOrNone(msg); ok {
		params = append(params, uid)
	} else {
		params = append(params, nil)
	}
	for _, enc := range []func(map[string]any) (string, bool, error){absorbedUIDsJSON, toolCallUIDsJSON} {
		text, ok, err := enc(msg)
		if err != nil {
			return nil, err
		}
		if ok {
			params = append(params, text)
		} else {
			params = append(params, nil)
		}
	}
	if uid, ok := toolCallUIDOrNone(msg); ok {
		params = append(params, uid)
	} else {
		params = append(params, nil)
	}
	return params, nil
}

// Ordered object view of a mapping value
func toPyObject(v any) pyObject {
	switch m := v.(type) {
	case pyObject:
		return append(pyObject{}, m...)
	case *pyjson.Object:
		out := pyObject{}
		for _, k := range m.Keys {
			out = append(out, pyKeyValue{k, m.Vals[k]})
		}
		return out
	case map[string]any:
		out := pyObject{}
		for _, k := range sortedKeys(m) {
			out = append(out, pyKeyValue{k, m[k]})
		}
		return out
	}
	return pyObject{}
}

// Object with the key set in place or appended like a dict assignment
func pyObjectSet(obj pyObject, key string, value any) pyObject {
	for i := range obj {
		if obj[i].Key == key {
			obj[i].Value = value
			return obj
		}
	}
	return append(obj, pyKeyValue{key, value})
}

// Object without the key like dict pop
func pyObjectDel(obj pyObject, key string) pyObject {
	for i := range obj {
		if obj[i].Key == key {
			return append(obj[:i:i], obj[i+1:]...)
		}
	}
	return obj
}

// Per occurrence tool call identity stamped at insert, results resolved through the batch index
func stampToolCallUIDs(msg map[string]any, toolCalls any, batchIndex map[string]string) {
	switch msg["role"] {
	case "assistant":
		if !pyTruthy(toolCalls) {
			return
		}
		uids := toolCallUIDMap(msg)
		for _, tc := range pyIter(toolCalls) {
			callID := CoalesceToolCallID(tc)
			if _, ok := uids.get(callID); callID != "" && !ok {
				uids = append(uids, pyKeyValue{callID, MintUID()})
			}
		}
		if len(uids) > 0 {
			msg[ToolCallUIDs] = uids
		}
		IndexToolCallUIDs(batchIndex, msg)
	case "user":
		clear(batchIndex)
	case "tool":
		if _, ok := toolCallUIDOrNone(msg); ok {
			return
		}
		if uid, ok := ResolveToolCallUID(batchIndex, msg["tool_call_id"]); ok && uid != "" {
			msg[ToolCallUID] = uid
		}
	}
}

// Bumps the session counters after an insert, unit baking the plus one literal into the SQL
func bumpSessionCounters(ctx context.Context, conn SQLiteConn, sessionID string, inserted, toolCalls int, unit bool) error {
	inc, params := "?", []any{inserted}
	if unit {
		inc, params = "1", nil
	}
	if toolCalls > 0 {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET message_count = message_count + "+inc+",\n                       tool_call_count = tool_call_count + ? WHERE id = ?",
			append(params, toolCalls, sessionID)...)
		return err
	}
	if inserted > 0 {
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET message_count = message_count + "+inc+" WHERE id = ?", append(params, sessionID)...)
		return err
	}
	return nil
}

// Guard options of one append, a nil TTL meaning the 300 second default
type AppendGuards struct {
	CompressionLockHolder *string
	TurnLeaseHolder       *string
	TurnLeaseTTLSeconds   *float64
}

// Appends one message dict with its role under the transcript guards, returning the row id after bumping counters
func (d *SessionDB) AppendMessage(ctx context.Context, sessionID string, fields map[string]any, guards AppendGuards) (int64, error) {
	msg := map[string]any{}
	for _, k := range appendMessageFields {
		msg[k] = fields[k]
	}
	role, _ := msg["role"].(string)
	meta, err := encodeDisplayMetadata(msg["display_metadata"])
	if err != nil {
		return 0, err
	}
	msg["display_metadata"] = meta
	toolCalls := parseToolCalls(msg["tool_calls"])
	timestamp := coerceMessageTimestamp(msg["timestamp"], now())
	StampMessageUID(msg)
	stampToolCallUIDs(msg, toolCalls, map[string]string{})
	params, err := messageRowParams(sessionID, role, msg, toolCalls, timestamp, true)
	if err != nil {
		return 0, err
	}
	var msgID int64
	patience := sessionTranscriptWritePatienceS
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		if err := d.checkTranscriptWriteGuards(ctx, conn, sessionID, transcriptGuards{compressionLockHolder: guards.CompressionLockHolder,
			turnLeaseHolder: guards.TurnLeaseHolder, turnLeaseTTLSeconds: guards.TurnLeaseTTLSeconds}); err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, hermesInsertMessageSQL, params...)
		if err != nil {
			return err
		}
		if msgID, err = res.LastInsertId(); err != nil {
			return err
		}
		return bumpSessionCounters(ctx, conn, sessionID, 1, toolCallsCount(toolCalls), true)
	}, &patience)
	return msgID, err
}

// Stamps display kind and metadata on the newest active row matching role and content, false when none matched
func (d *SessionDB) SetLatestMatchingMessageDisplayKind(ctx context.Context, sessionID, role, content, displayKind string, displayMetadata any) (bool, error) {
	if sessionID == "" || content == "" || displayKind == "" {
		return false, nil
	}
	matched := false
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		matched = false
		row, err := connRow(ctx, conn, "SELECT id FROM messages WHERE session_id = ? AND role = ? AND content = ? AND active = 1 ORDER BY id DESC LIMIT 1",
			sessionID, role, encodeMessageContent(content))
		if err != nil || row == nil {
			return err
		}
		meta, err := encodeDisplayMetadata(displayMetadata)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE messages SET display_kind = ?, display_metadata = ? WHERE id = ?",
			SanitizeSurrogates(displayKind), meta, row["id"]); err != nil {
			return err
		}
		matched = true
		return nil
	}, nil)
	return matched, err
}

// Well formed dict reactions stored under the reactions key
func reactionList(meta any) []any {
	reactions, _ := mappingGet(meta, ReactionsMetadataKey)
	list, ok := reactions.([]any)
	out := []any{}
	if !ok {
		return out
	}
	for _, r := range list {
		if isMapping(r) {
			out = append(out, r)
		}
	}
	return out
}

// Query and params addressing a row the client can see in the session resume lineage
func (d *SessionDB) reactionRowQuery(ctx context.Context, sessionID string, rowID int64) (string, []any, error) {
	lineage, err := d.resumeLineageIDs(ctx, sessionID)
	if err != nil {
		return "", nil, err
	}
	params := append([]any{rowID}, anyStrings(lineage)...)
	return strings.Replace(displayMetaRowSQL, "{ids}", placeholders(len(lineage)), 1), params, nil
}

// Sets or clears an author reaction with tapback semantics, false for a row outside the visible lineage
func (d *SessionDB) SetMessageReaction(ctx context.Context, sessionID string, rowID int64, emoji *string, author string) ([]any, bool, error) {
	if sessionID == "" {
		return nil, false, nil
	}
	query, params, err := d.reactionRowQuery(ctx, sessionID, rowID)
	if err != nil {
		return nil, false, err
	}
	var out []any
	found := false
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		out, found = nil, false
		row, err := connRow(ctx, conn, query, params...)
		if err != nil || row == nil {
			return err
		}
		meta := toPyObject(decodeDisplayMetadata(row["display_metadata"]))
		existing := reactionList(meta)
		reactions := []any{}
		var previous any
		for _, r := range existing {
			a, _ := mappingGet(r, "author")
			if a != author {
				reactions = append(reactions, r)
			} else if previous == nil {
				previous = r
			}
		}
		if emoji != nil && *emoji != "" {
			prevEmoji, _ := mappingGet(previous, "emoji")
			if previous == nil || prevEmoji != *emoji {
				reactions = append(reactions, pyObject{{"emoji", SanitizeSurrogates(*emoji)}, {"author", author}, {"at", now()}})
			}
		}
		if len(reactions) > 0 {
			meta = pyObjectSet(meta, ReactionsMetadataKey, reactions)
		} else {
			meta = pyObjectDel(meta, ReactionsMetadataKey)
		}
		var encoded any
		if len(meta) > 0 {
			if encoded, err = encodeDisplayMetadata(meta); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, setDisplayMetaSQL, encoded, rowID); err != nil {
			return err
		}
		out, found = reactions, true
		return nil
	}, nil)
	return out, found, err
}

// Reaction list persisted on one visible message row, never nil
func (d *SessionDB) GetMessageReactions(ctx context.Context, sessionID string, rowID int64) ([]any, error) {
	if sessionID == "" {
		return []any{}, nil
	}
	query, params, err := d.reactionRowQuery(ctx, sessionID, rowID)
	if err != nil {
		return nil, err
	}
	row, err := d.readOne(ctx, query, params...)
	if err != nil || row == nil {
		return []any{}, err
	}
	return reactionList(decodeDisplayMetadata(rowValue(row, "display_metadata"))), nil
}

// Author reactions not yet surfaced, marked seen in the same write so each is announced once
func (d *SessionDB) TakeUnseenReactions(ctx context.Context, sessionID, author string) ([]map[string]any, error) {
	if sessionID == "" {
		return []map[string]any{}, nil
	}
	lineage, err := d.resumeLineageIDs(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var pending []map[string]any
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		pending = []map[string]any{}
		rows, err := queryObjects(ctx, conn, "SELECT id, role, content, display_metadata FROM messages "+
			"WHERE session_id IN ("+placeholders(len(lineage))+")"+displayActiveClause+" "+
			"AND "+sqlJSONExtract("display_metadata", "$."+ReactionsMetadataKey)+" IS NOT NULL ORDER BY id", anyStrings(lineage)...)
		if err != nil {
			return err
		}
		for _, row := range rows {
			meta := decodeDisplayMetadata(rowValue(row, "display_metadata"))
			raw, _ := mappingGet(meta, ReactionsMetadataKey)
			reactions, ok := raw.([]any)
			if !ok {
				continue
			}
			changed := false
			for i, reaction := range reactions {
				obj, isObj := reaction.(pyObject)
				if !isObj {
					continue
				}
				a, _ := obj.get("author")
				seen, _ := obj.get("seen")
				if a != author || pyTruthy(seen) {
					continue
				}
				reactions[i] = pyObjectSet(obj, "seen", true)
				changed = true
				content := decodeMessageContent(rowValue(row, "content"))
				text, _ := content.(string)
				emoji, _ := obj.get("emoji")
				if !pyTruthy(emoji) {
					emoji = ""
				}
				pending = append(pending, map[string]any{"row_id": rowValue(row, "id"), "role": rowValue(row, "role"), "emoji": emoji, "text": text})
			}
			if changed {
				encoded, err := encodeDisplayMetadata(pyObjectSet(toPyObject(meta), ReactionsMetadataKey, reactions))
				if err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx, setDisplayMetaSQL, encoded, rowValue(row, "id")); err != nil {
					return err
				}
			}
		}
		return nil
	}, nil)
	return pending, err
}

// Row id of the newest active role message offset steps back, false when none or the role is not user or assistant
func (d *SessionDB) LatestMessageRowID(ctx context.Context, sessionID, role string, offset int, requireText bool) (int64, bool, error) {
	if sessionID == "" || (role != "user" && role != "assistant") || offset < 0 {
		return 0, false, nil
	}
	textFilter := ""
	if requireText {
		textFilter = "AND content IS NOT NULL AND TRIM(content) != '' "
	}
	row, err := d.readOne(ctx, "SELECT id FROM messages WHERE session_id = ? AND role = ? AND active = 1 "+textFilter+"ORDER BY id DESC LIMIT 1 OFFSET ?",
		sessionID, role, offset)
	if err != nil || row == nil {
		return 0, false, err
	}
	id, ok := rowInt(row, "id")
	return id, ok, nil
}

// Role of the newest active conversation row skipping session_meta and system bookkeeping, empty when none
func (d *SessionDB) LatestConversationRole(ctx context.Context, sessionID string) (string, error) {
	row, err := d.readOne(ctx, "SELECT role FROM messages WHERE session_id = ? AND active = 1 AND role NOT IN ('session_meta', 'system') ORDER BY id DESC LIMIT 1", sessionID)
	if err != nil || row == nil {
		return "", err
	}
	return pyStr(rowValue(row, "role")), nil
}

// Role of the active message at the row id in the session, false when absent
func (d *SessionDB) GetMessageRole(ctx context.Context, sessionID string, rowID int64) (string, bool, error) {
	if sessionID == "" {
		return "", false, nil
	}
	row, err := d.readOne(ctx, "SELECT role FROM messages WHERE id = ? AND session_id = ? AND active = 1", rowID, sessionID)
	if err != nil || row == nil {
		return "", false, err
	}
	return pyStr(rowValue(row, "role")), true, nil
}

// Whether a json reasoning item is a compaction checkpoint
func isCheckpointItem(item any) bool {
	kind, _ := mappingGet(item, "type")
	return isMapping(item) && kind == "compaction"
}

// Index of the last assistant message carrying a compaction checkpoint under the key, minus one when none
func newestCheckpointCarrier(messages []map[string]any, key string) int {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg == nil || msg["role"] != "assistant" {
			continue
		}
		if items, ok := msg[key].([]any); ok {
			for _, item := range items {
				if isCheckpointItem(item) {
					return i
				}
			}
		}
	}
	return -1
}

// Keeps only the newest checkpoint among inserted live messages carrying a row id
func pruneShadowedCheckpoints(ctx context.Context, conn SQLiteConn, sessionID string, live []map[string]any) error {
	carrier := newestCheckpointCarrier(live, "codex_reasoning_items")
	if carrier < 0 {
		return nil
	}
	rowID, ok := live[carrier]["_row_id"].(int64)
	if !ok {
		return nil
	}
	_, err := dropShadowedCheckpointRows(ctx, conn, sessionID, rowID)
	return err
}

// Rewrites older active assistant rows so only the carrier keeps a compaction checkpoint, returning rows rewritten
func dropShadowedCheckpointRows(ctx context.Context, conn SQLiteConn, sessionID string, carrierRowID int64) (int, error) {
	rows, err := queryObjects(ctx, conn, shadowedCheckpointRowsSQL, sessionID, carrierRowID)
	if err != nil {
		return 0, err
	}
	rewritten := 0
	for _, row := range rows {
		parsed, err := jsonOr(rowValue(row, "codex_reasoning_items"), nil, "Ignoring malformed codex_reasoning_items on message row")
		if err != nil {
			return rewritten, err
		}
		items, ok := parsed.([]any)
		if !ok {
			continue
		}
		kept := []any{}
		for _, item := range items {
			if !isCheckpointItem(item) {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(items) {
			continue
		}
		text, err := reasoningJSONText(kept)
		if err != nil {
			return rewritten, err
		}
		if _, err := conn.ExecContext(ctx, setCodexReasoningSQL, text, rowValue(row, "id")); err != nil {
			return rewritten, err
		}
		rewritten++
	}
	return rewritten, nil
}

// Ordered physical active ids of a session for rewind compare and set checks
func (d *SessionDB) GetActiveMessageIDs(ctx context.Context, sessionID string) ([]int64, error) {
	rows, err := d.readAll(ctx, activeIDsSQL, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		id, _ := rowInt(row, "id")
		out = append(out, id)
	}
	return out, nil
}

// Active message and tool call counts inside the caller transaction
func activeTranscriptCounts(ctx context.Context, conn SQLiteConn, sessionID string) (int, int, error) {
	rows, err := queryObjects(ctx, conn, "SELECT tool_calls FROM messages WHERE session_id = ? AND active = 1", sessionID)
	if err != nil {
		return 0, 0, err
	}
	total := 0
	for _, row := range rows {
		total += toolCallsLen(rowValue(row, "tool_calls"), 1)
	}
	return len(rows), total, nil
}

// Message count of one session, every session when the id is empty
func (d *SessionDB) MessageCount(ctx context.Context, sessionID string) (int64, error) {
	query, args := "SELECT COUNT(*) AS n FROM messages", []any{}
	if sessionID != "" {
		query, args = query+" WHERE session_id = ?", []any{sessionID}
	}
	row, err := d.readOne(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := rowInt(row, "n")
	return n, nil
}

// Whether an accepted user input row carries the gateway input owner marker
func (d *SessionDB) HasGatewayInputOwner(ctx context.Context, sessionID, owner string) (bool, error) {
	row, err := d.readOne(ctx, "SELECT 1 FROM messages WHERE session_id = ? AND role = 'user' "+
		"AND observed = 0 AND (active = 1 OR compacted = 1) "+
		"AND CASE WHEN json_valid(display_metadata) "+
		"THEN json_extract(display_metadata, '$.gateway_input_owner') END = ? LIMIT 1", sessionID, owner)
	return row != nil, err
}

// Whether the platform message id was already persisted in the session
func (d *SessionDB) HasPlatformMessageID(ctx context.Context, sessionID, platformMessageID string) (bool, error) {
	row, err := d.readOne(ctx, "SELECT 1 FROM messages WHERE session_id = ? AND platform_message_id = ? LIMIT 1", sessionID, platformMessageID)
	return row != nil, err
}

// Conversation boundaries the peer has crossed, false when it never reset
func (d *SessionDB) LatestConversationBoundary(ctx context.Context, sessionKey, source string) (int64, bool, error) {
	if sessionKey == "" || source == "" {
		return 0, false, nil
	}
	row, err := d.readOne(ctx, "SELECT generation FROM conversation_generations WHERE source = ? AND session_key = ?", source, sessionKey)
	if err != nil || row == nil {
		return 0, false, err
	}
	generation, _ := rowInt(row, "generation")
	return generation, generation > 0, nil
}

// Deletes every message of a session and resets its counters
func (d *SessionDB) ClearMessages(ctx context.Context, sessionID string) error {
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, resetCountersSQL, sessionID)
		return err
	}, nil)
}

// Keys of a map in sorted order
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
