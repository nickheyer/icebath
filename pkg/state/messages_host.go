package state

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Columns the message insert writes, in statement order
var messageWriteColumns = []string{"session_id", "role", "content", "tool_call_id", "tool_calls", "tool_name",
	"effect_disposition", "timestamp", "token_count", "finish_reason", "reasoning", "reasoning_content",
	"reasoning_details", "codex_reasoning_items", "codex_message_items", "platform_message_id", "observed",
	"_compressed_summary", "active", "api_content", "display_kind", "display_metadata", "display_identity",
	"message_uid", "absorbed_message_uids", "tool_call_uids", "tool_call_uid"}

// Every message column the row decoder knows, the written ones plus id, compacted and display_order
var messageSchemaKeys = func() map[string]bool {
	keys := map[string]bool{"id": true, "compacted": true, "display_order": true}
	for _, c := range messageWriteColumns {
		keys[c] = true
	}
	return keys
}()

// Fallback jsonOr hands back when the fallback must be distinguishable from JSON null
var jsonInvalid = &struct{ name string }{"invalid"}

// Reverses the reserved prefix JSON encoding of message content, a malformed marker returning the raw string
func decodeMessageContent(content any) any {
	if s, ok := content.(string); ok && strings.HasPrefix(s, contentJSONPrefix) {
		v, _ := jsonOr(s[len(contentJSONPrefix):], s, "Failed to decode JSON-encoded message content; returning raw string")
		return v
	}
	return content
}

// Display metadata column decoded to an object, unwrapping the second string layer of pre guard rows
func decodeDisplayMetadata(raw any) any {
	if raw == nil {
		return nil
	}
	meta := raw
	for range 2 {
		if s, ok := meta.(string); ok {
			meta, _ = jsonOr(s, jsonInvalid, "Ignoring invalid display metadata on message row")
		}
	}
	if meta == jsonInvalid {
		return nil
	}
	switch meta.(type) {
	case pyObject, *pyjson.Object:
		return meta
	}
	slog.Warn("Ignoring non-object display metadata on message row")
	return nil
}

// Message row with content, tool_calls and display_metadata decoded and unknown binary columns dropped
func rowToMessageDict(row *pyjson.Object, warnContext string, summaryFlag bool) (*pyjson.Object, error) {
	msg := pyjson.NewObject()
	for _, k := range row.Keys {
		msg.Set(k, row.Vals[k])
	}
	msg.Delete("display_identity")
	msg.Delete("display_order")
	if summaryFlag {
		flag, ok := msg.Get("_compressed_summary")
		msg.Delete("_compressed_summary")
		if ok && pyTruthy(flag) {
			msg.Set("_compressed_summary", true)
		}
	}
	content, _ := msg.Get("content")
	msg.Set("content", decodeMessageContent(content))
	if toolCalls, _ := msg.Get("tool_calls"); pyTruthy(toolCalls) {
		decoded, err := jsonOr(toolCalls, []any{}, fmt.Sprintf("Failed to deserialize tool_calls in %s, falling back to []", warnContext))
		if err != nil {
			return nil, err
		}
		msg.Set("tool_calls", decoded)
	}
	if meta, ok := msg.Get("display_metadata"); ok && meta != nil {
		msg.Set("display_metadata", decodeDisplayMetadata(meta))
	}
	for _, key := range append([]string(nil), msg.Keys...) {
		if _, isBytes := msg.Vals[key].([]byte); isBytes && !messageSchemaKeys[key] {
			msg.Delete(key)
		}
	}
	return msg, nil
}

// Up to window messages either side of an anchor id in ascending order, empty for an anchor of another session
func getMessagesAround(ctx context.Context, readCtx func(context.Context, bool, func(SQLiteConn) error) error,
	sessionID string, aroundMessageID int64, window int) (*pyjson.Object, error) {
	window = max(window, 0)
	var beforeRows, afterRows []*pyjson.Object
	found := false
	err := readCtx(ctx, true, func(conn SQLiteConn) error {
		hit, err := fetchOne(ctx, conn, "SELECT 1 FROM messages WHERE id = ? AND session_id = ? LIMIT 1", aroundMessageID, sessionID)
		if err != nil || hit == nil {
			return err
		}
		found = true
		if beforeRows, err = queryObjects(ctx, conn, "SELECT * FROM messages WHERE session_id = ? AND id <= ? ORDER BY id DESC LIMIT ?",
			sessionID, aroundMessageID, window+1); err != nil {
			return err
		}
		afterRows, err = queryObjects(ctx, conn, "SELECT * FROM messages WHERE session_id = ? AND id > ? ORDER BY id ASC LIMIT ?",
			sessionID, aroundMessageID, window)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := pyjson.NewObject()
	if !found {
		out.Set("window", []any{})
		out.Set("messages_before", int64(0))
		out.Set("messages_after", int64(0))
		return out, nil
	}
	windowMsgs := []any{}
	ordered := make([]*pyjson.Object, 0, len(beforeRows)+len(afterRows))
	for i := len(beforeRows) - 1; i >= 0; i-- {
		ordered = append(ordered, beforeRows[i])
	}
	for _, r := range append(ordered, afterRows...) {
		msg, err := rowToMessageDict(r, "get_messages_around", false)
		if err != nil {
			return nil, err
		}
		windowMsgs = append(windowMsgs, msg)
	}
	out.Set("window", windowMsgs)
	out.Set("messages_before", int64(max(0, len(beforeRows)-1)))
	out.Set("messages_after", int64(len(afterRows)))
	return out, nil
}
