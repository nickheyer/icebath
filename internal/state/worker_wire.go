package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
)

const workerMessageMax = 1000

var workerMessageFields = map[string]bool{
	"role": true, "content": true, "tool_name": true, "tool_calls": true, "tool_call_id": true, "token_count": true,
	"finish_reason": true, "reasoning": true, "reasoning_content": true, "reasoning_details": true,
	"codex_reasoning_items": true, "codex_message_items": true, "platform_message_id": true, "message_id": true,
	"observed": true, "effect_disposition": true, "_compressed_summary": true, "timestamp": true, "api_content": true,
	"display_kind": true, "display_metadata": true, "_row_id": true, "_canonical_content": true, "_db_row_snapshot": true,
	"_canonical_row": true, "data_use": true, "_submit_row_session_id": true, "_merged_turn_prefix": true,
	"message_uid": true, "_absorbed_message_uids": true, "_tool_call_uids": true, "_tool_call_uid": true,
	"_merged_durable_rows": true, "_unnamed_durable_rows": true, "_retired_durable_rows": true,
	"_restored_from_store": true, "_absorbed_row_ids": true,
}

func workerFieldAllowed(key string, handoff bool) bool {
	return workerMessageFields[key] || (handoff && key == "_compressed_summary_has_user_turn")
}

func workerRole(role string) bool {
	return role == "user" || role == "assistant" || role == "system" || role == "tool"
}

func wireBind(raw json.RawMessage) any {
	if n, ok := jsonInt(raw); ok {
		return n
	}
	if jsonIsString(raw) {
		return jsonString(raw)
	}
	if f, ok := jsonNumber(raw); ok {
		return f
	}
	return nil
}

func jsonNumber(raw json.RawMessage) (float64, bool) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t[0] == '"' || t == "true" || t == "false" || t == "null" || t[0] == '{' || t[0] == '[' {
		return 0, false
	}
	var f float64
	if json.Unmarshal([]byte(t), &f) != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

func jsonBool(raw json.RawMessage) (bool, bool) {
	switch strings.TrimSpace(string(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func decodeWireMessages(ctx context.Context, c Conn, sessionID string, raw json.RawMessage, handoff bool) ([]*Message, error) {
	elements, ok := arrayElements(raw)
	if !ok || len(elements) > workerMessageMax {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	out := make([]*Message, 0, len(elements))
	for _, e := range elements {
		members, ok := objectMembers(e)
		if !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		for _, m := range members {
			if !workerFieldAllowed(m.key, handoff) {
				return nil, runtimeErr(ReasonInvalidParams)
			}
		}
		role, _ := memberValue(members, "role")
		if !jsonIsString(role) || !workerRole(jsonString(role)) {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		if rowID, has := memberValue(members, "_row_id"); has {
			var one int
			err := c.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE id=? AND session_id=?", wireBind(rowID), sessionID).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, runtimeErr(ReasonPermissionDenied)
			}
			if err != nil {
				return nil, err
			}
		}
		msg, err := decodeWireMessage(members)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, nil
}

func wireText(members []configMember, key string) (sql.Null[string], error) {
	v, has := memberValue(members, key)
	if !has || jsonIsNull(v) {
		return sql.Null[string]{}, nil
	}
	if !jsonIsString(v) {
		return sql.Null[string]{}, runtimeErr(ReasonInvalidParams)
	}
	return sql.Null[string]{V: jsonString(v), Valid: true}, nil
}

func wireRaw(members []configMember, key string) json.RawMessage {
	v, has := memberValue(members, key)
	if !has || jsonIsNull(v) {
		return nil
	}
	return v
}

func wireStrings(raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	elements, ok := arrayElements(raw)
	if !ok {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	out := make([]string, 0, len(elements))
	for _, e := range elements {
		if !jsonIsString(e) {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		out = append(out, jsonString(e))
	}
	return out, nil
}

func decodeWireMessage(members []configMember) (*Message, error) {
	m := &Message{}
	role, _ := memberValue(members, "role")
	m.Role = jsonString(role)
	if v, has := memberValue(members, "content"); has {
		m.Content = JSONContent(v)
	}
	texts := []struct {
		key string
		dst *sql.Null[string]
	}{{"tool_call_id", &m.ToolCallID}, {"tool_name", &m.ToolName}, {"effect_disposition", &m.EffectDisposition},
		{"finish_reason", &m.FinishReason}, {"reasoning", &m.Reasoning}, {"reasoning_content", &m.ReasoningContent},
		{"platform_message_id", &m.PlatformMessageID}, {"api_content", &m.APIContent}, {"display_kind", &m.DisplayKind}}
	for _, t := range texts {
		v, err := wireText(members, t.key)
		if err != nil {
			return nil, err
		}
		*t.dst = v
	}
	if !m.PlatformMessageID.Valid || m.PlatformMessageID.V == "" {
		alias, err := wireText(members, "message_id")
		if err != nil {
			return nil, err
		}
		if alias.Valid && alias.V != "" {
			m.PlatformMessageID = alias
		}
	}
	m.ToolCalls = wireRaw(members, "tool_calls")
	m.ReasoningDetails = wireRaw(members, "reasoning_details")
	m.CodexReasoningItems = wireRaw(members, "codex_reasoning_items")
	m.CodexMessageItems = wireRaw(members, "codex_message_items")
	m.DisplayMetadata = wireRaw(members, "display_metadata")
	if v := wireRaw(members, "token_count"); v != nil {
		n, ok := jsonInt(v)
		if !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		m.TokenCount = sql.Null[int64]{V: n, Valid: true}
	}
	if v := wireRaw(members, "timestamp"); v != nil {
		f, ok := jsonNumber(v)
		if !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		m.Timestamp = sql.Null[float64]{V: f, Valid: true}
	}
	m.Observed = jsonTruthy(wireRaw(members, "observed"))
	m.CompressedSummary = jsonTruthy(wireRaw(members, "_compressed_summary"))
	if v := wireRaw(members, "_row_id"); v != nil {
		if n, ok := jsonInt(v); ok {
			m.RowID = n
		}
	}
	if v := wireRaw(members, "_db_row_snapshot"); v != nil && jsonIsString(v) {
		m.Snapshot = jsonString(v)
	}
	if v := wireRaw(members, "message_uid"); v != nil && jsonIsString(v) {
		m.UID = jsonString(v)
	}
	if v := wireRaw(members, "_tool_call_uid"); v != nil && jsonIsString(v) {
		m.ToolCallUID = jsonString(v)
	}
	if v := wireRaw(members, "_tool_call_uids"); v != nil {
		m.ToolCallUIDs = DecodeToolCallUIDs(string(v))
	}
	absorbed, err := wireStrings(wireRaw(members, "_absorbed_message_uids"))
	if err != nil {
		return nil, err
	}
	m.AbsorbedUIDs = UIDList(absorbed)
	if m.DataUse, err = wireStrings(wireRaw(members, "data_use")); err != nil {
		return nil, err
	}
	if v, ok := jsonBool(wireRaw(members, "_restored_from_store")); ok && v {
		m.RestoredFromStore = true
	}
	if v := wireRaw(members, "_merged_durable_rows"); v != nil {
		n, _ := jsonInt(v)
		m.MergedDurableRows = int(n)
	}
	if v := wireRaw(members, "_unnamed_durable_rows"); v != nil {
		n, _ := jsonInt(v)
		m.UnnamedDurableRows = int(n)
	}
	if v := wireRaw(members, "_absorbed_row_ids"); v != nil {
		elements, _ := arrayElements(v)
		for _, e := range elements {
			if n, ok := jsonInt(e); ok && n > 0 && !containsInt64(m.AbsorbedRowIDs, n) {
				m.AbsorbedRowIDs = append(m.AbsorbedRowIDs, n)
			}
		}
	}
	if v := wireRaw(members, "_retired_durable_rows"); v != nil {
		elements, _ := arrayElements(v)
		for _, e := range elements {
			if row, ok := decodeRetiredRow(e); ok {
				m.RetiredDurableRows = append(m.RetiredDurableRows, row)
			}
		}
	}
	return m, nil
}

func containsInt64(values []int64, v int64) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

func decodeRetiredRow(raw json.RawMessage) (RetiredRow, bool) {
	members, ok := objectMembers(raw)
	if !ok {
		return RetiredRow{}, false
	}
	role, _ := memberValue(members, "role")
	row := RetiredRow{Role: jsonString(role)}
	if v, has := memberValue(members, "content"); has {
		row.Content = JSONContent(v)
	}
	if v, has := memberValue(members, "tool_call_id"); has && jsonIsString(v) {
		row.ToolCallID = jsonString(v)
	}
	if v, has := memberValue(members, "tool_call_ids"); has {
		elements, _ := arrayElements(v)
		for _, e := range elements {
			row.ToolCallIDs = append(row.ToolCallIDs, jsonString(e))
		}
	}
	row.Own = jsonTruthy(wireRaw(members, "_own_row"))
	return row, true
}

func decodeWireHeld(raw json.RawMessage) ([]Held, error) {
	elements, ok := arrayElements(raw)
	if !ok {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	out := make([]Held, 0, len(elements))
	for _, e := range elements {
		members, ok := objectMembers(e)
		if !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		if jsonTruthy(wireRaw(members, "_retired_row")) {
			row, _ := decodeRetiredRow(e)
			out = append(out, Held{Retired: &row})
			continue
		}
		msg, err := decodeWireMessage(members)
		if err != nil {
			return nil, err
		}
		out = append(out, Held{Message: msg})
	}
	return out, nil
}

func decodeWireMessageList(raw json.RawMessage) ([]*Message, error) {
	elements, ok := arrayElements(raw)
	if !ok {
		return nil, runtimeErr(ReasonInvalidParams)
	}
	out := make([]*Message, 0, len(elements))
	for _, e := range elements {
		members, ok := objectMembers(e)
		if !ok {
			return nil, runtimeErr(ReasonInvalidParams)
		}
		msg, err := decodeWireMessage(members)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, nil
}

type wireObject struct {
	members []configMember
	err     error
}

func (w *wireObject) set(key string, v any) {
	if w.err != nil {
		return
	}
	var raw json.RawMessage
	switch t := v.(type) {
	case json.RawMessage:
		raw = t
		if raw == nil {
			raw = json.RawMessage("null")
		}
	case json.Marshaler:
		enc, err := t.MarshalJSON()
		if err != nil {
			w.err = err
			return
		}
		raw = enc
	case ToolCallUIDs:
		enc, err := json.Marshal(t)
		if err != nil {
			w.err = err
			return
		}
		raw = enc
	default:
		enc, err := fileutil.EncodeJSON(v, fileutil.JSONStyle{Tight: true})
		if err != nil {
			w.err = err
			return
		}
		raw = enc
	}
	w.members = setConfigMember(w.members, key, raw)
}

func (w *wireObject) raw() (json.RawMessage, error) {
	if w.err != nil {
		return nil, w.err
	}
	return rawObject(w.members), nil
}

func nullTextValue(v sql.Null[string]) any {
	if !v.Valid {
		return nil
	}
	return v.V
}

func setIdentity(w *wireObject, m *Message) {
	if m.UID != "" {
		w.set("message_uid", m.UID)
	}
	if len(m.AbsorbedUIDs) > 0 {
		w.set("_absorbed_message_uids", m.AbsorbedUIDs)
	}
	if len(m.ToolCallUIDs) > 0 {
		w.set("_tool_call_uids", m.ToolCallUIDs)
	}
	if m.ToolCallUID != "" {
		w.set("_tool_call_uid", m.ToolCallUID)
	}
}

func encodeConversationMessage(m *Message) (json.RawMessage, error) {
	w := &wireObject{}
	w.set("role", m.Role)
	w.set("content", m.Content)
	if m.Persisted {
		w.set("_db_persisted", true)
	}
	if m.Snapshot != "" {
		w.set("_db_row_snapshot", m.Snapshot)
	}
	if m.RowID > 0 {
		w.set("_row_id", m.RowID)
	}
	setIdentity(w, m)
	if m.APIContent.Valid && m.APIContent.V != "" {
		w.set("api_content", m.APIContent.V)
	}
	if m.DisplayKind.Valid && m.DisplayKind.V != "" {
		w.set("display_kind", m.DisplayKind.V)
	}
	if m.DisplayMetadata != nil {
		w.set("display_metadata", m.DisplayMetadata)
	}
	if len(m.DataUse) > 0 {
		w.set("data_use", m.DataUse)
	}
	if m.CompressedSummary {
		w.set("_compressed_summary", true)
	}
	if m.Timestamp.Valid && m.Timestamp.V != 0 {
		w.set("timestamp", m.Timestamp.V)
	}
	for _, f := range []struct {
		key string
		v   sql.Null[string]
	}{{"tool_call_id", m.ToolCallID}, {"tool_name", m.ToolName}, {"effect_disposition", m.EffectDisposition}} {
		if f.v.Valid && f.v.V != "" {
			w.set(f.key, f.v.V)
		}
	}
	if m.ToolCalls != nil {
		w.set("tool_calls", m.ToolCalls)
	}
	if m.PlatformMessageID.Valid && m.PlatformMessageID.V != "" {
		w.set("message_id", m.PlatformMessageID.V)
	}
	if m.Observed {
		w.set("observed", true)
	}
	if m.Role == "assistant" {
		if m.FinishReason.Valid && m.FinishReason.V != "" {
			w.set("finish_reason", m.FinishReason.V)
		}
		if m.Reasoning.Valid && m.Reasoning.V != "" {
			w.set("reasoning", m.Reasoning.V)
		}
		if m.ReasoningContent.Valid {
			w.set("reasoning_content", m.ReasoningContent.V)
		}
		for _, f := range []struct {
			key string
			v   json.RawMessage
		}{{"reasoning_details", m.ReasoningDetails}, {"codex_reasoning_items", m.CodexReasoningItems}, {"codex_message_items", m.CodexMessageItems}} {
			if f.v != nil {
				w.set(f.key, f.v)
			}
		}
	}
	return w.raw()
}

func encodeRepairRow(m *Message) *wireObject {
	w := &wireObject{}
	w.set("role", m.Role)
	w.set("content", m.Content)
	for _, f := range []struct {
		key string
		v   sql.Null[string]
	}{{"tool_call_id", m.ToolCallID}, {"tool_name", m.ToolName}, {"effect_disposition", m.EffectDisposition}} {
		if f.v.Valid {
			w.set(f.key, f.v.V)
		}
	}
	if m.TokenCount.Valid {
		w.set("token_count", m.TokenCount.V)
	}
	if m.FinishReason.Valid {
		w.set("finish_reason", m.FinishReason.V)
	}
	if m.ToolCalls != nil {
		w.set("tool_calls", m.ToolCalls)
	}
	if m.PlatformMessageID.Valid {
		w.set("message_id", m.PlatformMessageID.V)
		w.set("platform_message_id", m.PlatformMessageID.V)
	}
	if m.Observed {
		w.set("observed", true)
	}
	if m.CompressedSummary {
		w.set("_compressed_summary", true)
	}
	setIdentity(w, m)
	if m.APIContent.Valid {
		w.set("api_content", m.APIContent.V)
	}
	if m.DisplayKind.Valid {
		w.set("display_kind", m.DisplayKind.V)
	}
	if m.DisplayMetadata != nil {
		w.set("display_metadata", m.DisplayMetadata)
	}
	if m.Role == "assistant" {
		w.set("reasoning", nullTextValue(m.Reasoning))
		w.set("reasoning_content", nullTextValue(m.ReasoningContent))
		for _, f := range []struct {
			key string
			v   json.RawMessage
		}{{"reasoning_details", m.ReasoningDetails}, {"codex_reasoning_items", m.CodexReasoningItems}, {"codex_message_items", m.CodexMessageItems}} {
			if f.v != nil {
				w.set(f.key, f.v)
			}
		}
	}
	return w
}

func encodeCanonical(cr *CanonicalRow) (json.RawMessage, error) {
	if cr == nil {
		return json.RawMessage("null"), nil
	}
	w := &wireObject{}
	switch {
	case cr.ContentOnly:
		w.set("content", cr.Message.Content)
		w.set("_content_only", true)
	case cr.MetadataOnly:
		if cr.Message.DisplayKind.Valid {
			w.set("display_kind", cr.Message.DisplayKind.V)
		}
		if cr.Message.DisplayMetadata != nil {
			w.set("display_metadata", cr.Message.DisplayMetadata)
		}
		w.set("_metadata_only", true)
	default:
		w = encodeRepairRow(&cr.Message)
	}
	return w.raw()
}

func rowAnnotations(messages []*Message) (json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(messages))
	for _, m := range messages {
		w := &wireObject{}
		if m.RowID > 0 {
			w.set("_row_id", m.RowID)
		} else {
			w.set("_row_id", nil)
		}
		if m.Timestamp.Valid {
			w.set("timestamp", m.Timestamp.V)
		} else {
			w.set("timestamp", nil)
		}
		if m.Snapshot != "" {
			w.set("_db_row_snapshot", m.Snapshot)
		} else {
			w.set("_db_row_snapshot", nil)
		}
		canonical, err := encodeCanonical(m.Canonical)
		if err != nil {
			return nil, err
		}
		w.set("_canonical_row", canonical)
		if m.UID != "" {
			w.set("message_uid", m.UID)
		} else {
			w.set("message_uid", nil)
		}
		if len(m.ToolCallUIDs) > 0 {
			w.set("_tool_call_uids", m.ToolCallUIDs)
		} else {
			w.set("_tool_call_uids", nil)
		}
		if m.ToolCallUID != "" {
			w.set("_tool_call_uid", m.ToolCallUID)
		} else {
			w.set("_tool_call_uid", nil)
		}
		raw, err := w.raw()
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return rawArray(out), nil
}

func cellJSON(v any) (json.RawMessage, error) {
	switch t := v.(type) {
	case nil:
		return json.RawMessage("null"), nil
	case []byte:
		return nil, runtimeErr(ReasonInvalidParams)
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return nil, runtimeErr(ReasonInvalidParams)
		}
	}
	return fileutil.EncodeJSON(v, fileutil.JSONStyle{Tight: true})
}

func scanRowObjects(rows *sql.Rows) ([][]configMember, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]configMember
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		members := make([]configMember, 0, len(cols))
		for i, col := range cols {
			raw, err := cellJSON(vals[i])
			if err != nil {
				return nil, err
			}
			members = setConfigMember(members, col, raw)
		}
		out = append(out, members)
	}
	return out, rows.Err()
}

func sessionRowObject(ctx context.Context, c Conn, sessionID string) (json.RawMessage, bool, error) {
	rows, err := c.QueryContext(ctx, "SELECT s.*, COALESCE(sp.prompt, s.system_prompt) AS _system_prompt_resolved, "+
		"COALESCE(tp.prompt, s.tool_names) AS _tool_names_resolved "+
		"FROM sessions s LEFT JOIN system_prompts sp ON sp.hash=s.system_prompt_hash "+
		"LEFT JOIN system_prompts tp ON tp.hash=s.tool_names WHERE s.id=?", sessionID)
	if err != nil {
		return nil, false, err
	}
	objects, err := scanRowObjects(rows)
	if err != nil || len(objects) == 0 {
		return nil, false, err
	}
	data := objects[0]
	for _, column := range []string{"system_prompt", "tool_names"} {
		resolved, ok := memberValue(data, "_"+column+"_resolved")
		if !ok {
			continue
		}
		data = dropMember(data, "_"+column+"_resolved")
		if _, has := memberValue(data, column); has {
			data = setConfigMember(data, column, resolved)
		}
	}
	raw, _ := memberValue(data, "model_config")
	cfg, _ := objectMembers(json.RawMessage(jsonString(raw)))
	for _, key := range []string{"_reset_from", "_branched_from"} {
		v, _ := memberValue(cfg, key)
		if text := strings.TrimSpace(jsonString(v)); jsonIsString(v) && text != "" {
			enc, err := json.Marshal(text)
			if err != nil {
				return nil, false, err
			}
			data = setConfigMember(data, key, enc)
		}
	}
	return rawObject(data), true, nil
}

func requireSessionObject(ctx context.Context, c Conn, sessionID string) (json.RawMessage, []configMember, error) {
	raw, ok, err := sessionRowObject(ctx, c, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("session %s has no row", sessionID)
	}
	members, _ := objectMembers(raw)
	return raw, members, nil
}

func valueResult(v any) (json.RawMessage, error) {
	w := &wireObject{}
	w.set("value", v)
	return w.raw()
}
