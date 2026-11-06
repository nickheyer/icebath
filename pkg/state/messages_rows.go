package state

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Serializes one live message with the exact durable shape the INSERT writes
func (d *SessionDB) serializedMessageRow(sessionID string, msg map[string]any, timestamp float64) (map[string]any, error) {
	role := "unknown"
	if r, ok := msg["role"]; ok {
		role, _ = r.(string)
	}
	params, err := messageRowParams(sessionID, role, msg, parseToolCalls(msg["tool_calls"]), timestamp, role == "assistant")
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(messageWriteColumns))
	for i, column := range messageWriteColumns {
		if i < len(params) {
			out[column] = params[i]
		}
	}
	return out, nil
}

// Decodes one durable row without replay dedupe, alternation repair or content stripping
func (d *SessionDB) decodedRepairRow(row map[string]any) (map[string]any, error) {
	msg := map[string]any{"role": row["role"], "content": decodeMessageContent(row["content"])}
	for _, column := range []string{"tool_call_id", "tool_name", "effect_disposition", "token_count", "finish_reason"} {
		if row[column] != nil {
			msg[column] = row[column]
		}
	}
	if pyTruthy(row["tool_calls"]) {
		v, err := jsonOr(row["tool_calls"], []any{}, "Failed to deserialize repaired tool_calls, falling back to []")
		if err != nil {
			return nil, err
		}
		msg["tool_calls"] = v
	}
	if row["platform_message_id"] != nil {
		msg["message_id"] = row["platform_message_id"]
		msg["platform_message_id"] = row["platform_message_id"]
	}
	if pyTruthy(row["observed"]) {
		msg["observed"] = true
	}
	if pyTruthy(row["_compressed_summary"]) {
		msg["_compressed_summary"] = true
	}
	restoreIdentityColumns(row, msg)
	if row["api_content"] != nil {
		msg["api_content"] = row["api_content"]
	}
	if row["display_kind"] != nil {
		msg["display_kind"] = row["display_kind"]
	}
	if row["display_metadata"] != nil {
		if meta := decodeDisplayMetadata(row["display_metadata"]); meta != nil {
			msg["display_metadata"] = meta
		}
	}
	if row["role"] == "assistant" {
		for _, column := range []string{"reasoning", "reasoning_content"} {
			if row[column] != nil {
				msg[column] = row[column]
			}
		}
		for _, column := range []string{"reasoning_details", "codex_reasoning_items", "codex_message_items"} {
			if pyTruthy(row[column]) {
				v, err := jsonOr(row[column], nil, "Failed to deserialize repaired "+column+", falling back to None")
				if err != nil {
					return nil, err
				}
				msg[column] = v
			}
		}
	}
	return msg, nil
}

// Content as loaded rows hand it to callers, user and assistant strings sanitized and stripped
func loadedViewContent(role string, content any) any {
	if s, ok := content.(string); ok && (role == "user" || role == "assistant") {
		return pyStrip(SanitizeContext(s))
	}
	return content
}

// Codec the batch repair runs a session's transcript through
func (d *SessionDB) transcriptCodec(sessionID string) transcriptCodec {
	return transcriptCodec{
		encode: encodeMessageContent,
		decode: decodeMessageContent,
		serialize: func(msg map[string]any, ts float64) (map[string]any, error) {
			return d.serializedMessageRow(sessionID, msg, ts)
		},
		decodeRow: d.decodedRepairRow,
	}
}

// Live keys a transcript write stamps on caller dicts and restores when an attempt rolls back
var rowStateKeys = []string{"_row_id", DBRowSnapshot, "timestamp"}

// Appends messages in one write transaction with the guards run once, chunking large copies, returning the inserted count
func (d *SessionDB) AppendMessagesBatch(ctx context.Context, sessionID string, messages []map[string]any, guards AppendGuards, chunkRows *int) (int, error) {
	if len(messages) == 0 {
		return 0, nil
	}
	if chunkRows != nil && len(messages) > *chunkRows {
		index := map[string]string{}
		for _, msg := range messages {
			stampToolCallUIDs(msg, parseToolCalls(msg["tool_calls"]), index)
		}
		total := 0
		for start := 0; start < len(messages); start += *chunkRows {
			n, err := d.AppendMessagesBatch(ctx, sessionID, messages[start:min(start+*chunkRows, len(messages))], guards, nil)
			if err != nil {
				return total, err
			}
			total += n
		}
		return total, nil
	}
	var inserted int
	patience := sessionTranscriptWritePatienceS
	err := d.executeTranscriptWrite(ctx, func(conn SQLiteConn) error {
		n, err := d.appendMessagesInTransaction(ctx, conn, sessionID, messages, guards)
		inserted = n
		return err
	}, messages, &patience)
	return inserted, err
}

// Guards, repairs row addressed rewrites, inserts the rest and bumps counters inside the caller transaction
func (d *SessionDB) appendMessagesInTransaction(ctx context.Context, conn SQLiteConn, sessionID string, messages []map[string]any, guards AppendGuards) (int, error) {
	if err := d.checkTranscriptWriteGuards(ctx, conn, sessionID, transcriptGuards{compressionLockHolder: guards.CompressionLockHolder,
		turnLeaseHolder: guards.TurnLeaseHolder, turnLeaseTTLSeconds: guards.TurnLeaseTTLSeconds}); err != nil {
		return 0, err
	}
	rows, err := resolveAndRepairTranscriptBatch(ctx, conn, sessionID, messages, d.transcriptCodec(sessionID))
	if err != nil {
		return 0, err
	}
	inserted, toolCalls, err := d.insertMessageRows(ctx, conn, sessionID, rows, true)
	if err != nil {
		return 0, err
	}
	return inserted, bumpSessionCounters(ctx, conn, sessionID, inserted, toolCalls, false)
}

// Runs a write whose callback stamps row state on caller dicts, restoring that state before each attempt and after failure
func (d *SessionDB) executeTranscriptWrite(ctx context.Context, fn func(conn SQLiteConn) error, messages []map[string]any, patienceS *float64) error {
	type saved struct {
		value   any
		present bool
	}
	pre := make([][]saved, len(messages))
	for i, msg := range messages {
		for _, key := range rowStateKeys {
			v, ok := msg[key]
			pre[i] = append(pre[i], saved{v, ok})
		}
	}
	restore := func() {
		for i, msg := range messages {
			delete(msg, CanonicalRow)
			for j, key := range rowStateKeys {
				if pre[i][j].present {
					msg[key] = pre[i][j].value
				} else {
					delete(msg, key)
				}
			}
		}
	}
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		restore()
		return fn(conn)
	}, patienceS)
	if err != nil {
		restore()
	}
	return err
}

// Python hash and equality bucket of scalar row values, numbers equal across int and float
func scalarKey(values ...any) string {
	var b strings.Builder
	for _, v := range values {
		switch x := v.(type) {
		case nil:
			b.WriteString("N|")
		case string:
			fmt.Fprintf(&b, "S%d:%s|", len(x), x)
		case []byte:
			fmt.Fprintf(&b, "B%d:%s|", len(x), x)
		case float64:
			if x == math.Trunc(x) && !math.IsInf(x, 0) {
				fmt.Fprintf(&b, "I%s|", new(big.Float).SetFloat64(x).Text('f', 0))
			} else {
				fmt.Fprintf(&b, "F%s|", pyrepr.Float(x))
			}
		default:
			if n, ok := pyIntID(x); ok {
				fmt.Fprintf(&b, "I%d|", n)
			} else {
				fmt.Fprintf(&b, "R%s|", pyrepr.Repr(x))
			}
		}
	}
	return b.String()
}

// Adopts the parent's durable row timestamps onto carried handoff rows so their display identity survives
func (d *SessionDB) carryParentTimestamps(ctx context.Context, conn SQLiteConn, parentSessionID string, messages []map[string]any) {
	var candidates []int
	for i, msg := range messages {
		if msg == nil {
			continue
		}
		if _, ok := pydatetime.CoerceEpoch(msg["timestamp"], "", "message timestamp"); !ok {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return
	}
	rows, err := connRows(ctx, conn, "SELECT id, role, content, timestamp, tool_call_id, tool_calls, tool_name FROM messages WHERE session_id = ? AND active = 1 ORDER BY id", parentSessionID)
	if err != nil {
		return
	}
	donors := map[string][]any{}
	for _, row := range rows {
		key := scalarKey(row["role"], row["content"], row["tool_call_id"], row["tool_calls"], row["tool_name"])
		donors[key] = append(donors[key], row["timestamp"])
	}
	for _, i := range candidates {
		msg := messages[i]
		toolCalls := parseToolCalls(msg["tool_calls"])
		var encodedCalls any
		if pyTruthy(toolCalls) {
			text, err := pyDumps(toolCalls, pyJSONDefault)
			if err != nil {
				return
			}
			encodedCalls = text
		}
		role := any("unknown")
		if r, ok := msg["role"]; ok {
			role = r
		}
		key := scalarKey(role, encodeMessageContent(msg["content"]), msg["tool_call_id"], encodedCalls, scrubSurrogates(msg["tool_name"]))
		if queue := donors[key]; len(queue) > 0 {
			msg["timestamp"] = queue[0]
			donors[key] = queue[1:]
		}
	}
}

// Inserts messages as fresh active rows in the caller transaction, returning inserted and tool call counts
func (d *SessionDB) insertMessageRows(ctx context.Context, conn SQLiteConn, sessionID string, messages []map[string]any, pruneCheckpoints bool) (int, int, error) {
	nowTS := now()
	inserted, toolCallsTotal := 0, 0
	batchIndex := map[string]string{}
	for _, msg := range messages {
		role := "unknown"
		if r, ok := msg["role"]; ok {
			role, _ = r.(string)
		}
		toolCalls := parseToolCalls(msg["tool_calls"])
		timestamp := coerceMessageTimestamp(msg["timestamp"], nowTS)
		StampMessageUID(msg)
		stampToolCallUIDs(msg, toolCalls, batchIndex)
		params, err := messageRowParams(sessionID, role, msg, toolCalls, timestamp, role == "assistant")
		if err != nil {
			return 0, 0, err
		}
		res, err := conn.ExecContext(ctx, hermesInsertMessageSQL, params...)
		if err != nil {
			return 0, 0, err
		}
		msg["timestamp"] = timestamp
		if id, err := res.LastInsertId(); err == nil {
			msg["_row_id"] = id
		}
		inserted++
		toolCallsTotal += toolCallsCount(toolCalls)
		nowTS = math.Max(nowTS, timestamp) + 1e-6
	}
	if pruneCheckpoints {
		if err := pruneShadowedCheckpoints(ctx, conn, sessionID, messages); err != nil {
			return 0, 0, err
		}
	}
	if err := stampInsertedRowSnapshots(ctx, conn, sessionID, messages); err != nil {
		return 0, 0, err
	}
	return inserted, toolCallsTotal, nil
}
