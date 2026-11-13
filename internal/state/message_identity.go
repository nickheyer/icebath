package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Unique non-empty uids in their first order
func UIDList(values []string) []string {
	var out []string
	for _, v := range values {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// Uid list column text decoded, an undecodable value logged and read as none, non-text members dropped
func DecodeUIDList(text string) []string {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		var probe any
		if json.Unmarshal([]byte(text), &probe) != nil {
			slog.Warn("Failed to deserialize a message uid list, falling back to []")
		}
		return nil
	}
	var values []string
	for _, r := range raw {
		var s string
		if json.Unmarshal(r, &s) == nil {
			values = append(values, s)
		}
	}
	return UIDList(values)
}

// Absorbed uids column text of uids, false when none remain
func EncodeAbsorbedUIDs(uids []string) (string, bool, error) {
	list := UIDList(uids)
	if len(list) == 0 {
		return "", false, nil
	}
	enc, err := fileutil.EncodeJSON(list, fileutil.JSONStyle{ASCII: true})
	return string(enc), err == nil, err
}

// Stored uid of one provider call id, one uid or one per occurrence of an id repeated inside a row
type ToolCallUID struct {
	One string
	// Uids per occurrence, set when the column holds a list for this call id
	Many []string
}

// Occurrence uids, a single uid read as a list of one
func (u ToolCallUID) occurrences() []string {
	if u.Many != nil {
		return u.Many
	}
	return []string{u.One}
}

// Encodes as a string or a list the way the column stores it
func (u ToolCallUID) MarshalJSON() ([]byte, error) {
	if u.Many != nil {
		return json.Marshal(u.Many)
	}
	return json.Marshal(u.One)
}

// Tool call uid column, provider call id to its stored uid
type ToolCallUIDs map[string]ToolCallUID

// Tool call uid column text decoded, keeping non-empty string uids and non-empty lists of them
func DecodeToolCallUIDs(text string) ToolCallUIDs {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		var probe any
		if json.Unmarshal([]byte(text), &probe) != nil {
			slog.Warn("Failed to deserialize a tool-call uid map, falling back to {}")
		}
		return ToolCallUIDs{}
	}
	out := ToolCallUIDs{}
	for k, v := range raw {
		if k == "" {
			continue
		}
		var one string
		if json.Unmarshal(v, &one) == nil {
			if one != "" {
				out[k] = ToolCallUID{One: one}
			}
			continue
		}
		var many []any
		if json.Unmarshal(v, &many) != nil || len(many) == 0 {
			continue
		}
		list := make([]string, 0, len(many))
		for _, m := range many {
			s, ok := m.(string)
			if !ok || s == "" {
				list = nil
				break
			}
			list = append(list, s)
		}
		if list != nil {
			out[k] = ToolCallUID{Many: list}
		}
	}
	return out
}

// Tool call uid column text with keys sorted, false when the map is empty
func EncodeToolCallUIDs(m ToolCallUIDs) (string, bool, error) {
	if len(m) == 0 {
		return "", false, nil
	}
	values := make(map[string]any, len(m))
	for k, v := range m {
		if v.Many != nil {
			values[k] = v.Many
		} else {
			values[k] = v.One
		}
	}
	enc, err := fileutil.EncodeJSON(values, fileutil.JSONStyle{Sorted: true, ASCII: true})
	return string(enc), err == nil, err
}

// Reports a live per occurrence list that starts with every stored occurrence, a fold not yet written
func liveExtends(live ToolCallUID, present bool, stored ToolCallUID) bool {
	if !present || live.Many == nil {
		return false
	}
	want := stored.occurrences()
	return len(live.Many) >= len(want) && slices.Equal(live.Many[:len(want)], want)
}

// Live map plus the stored uid of each call it still names but lost, so a row rewrite keeps them
func FillMissingToolCallUIDs(stored, live ToolCallUIDs, callIDs []string) ToolCallUIDs {
	merged := ToolCallUIDs{}
	for k, v := range live {
		merged[k] = v
	}
	added := false
	for k, v := range stored {
		if _, have := live[k]; !have && slices.Contains(callIDs, k) {
			merged[k] = v
			added = true
		}
	}
	if !added {
		return live
	}
	return merged
}

// Stored uids restored onto a live map, the stored value winning unless a live list already extends it
func RestoreToolCallUIDs(live, stored ToolCallUIDs) ToolCallUIDs {
	merged := ToolCallUIDs{}
	for k, v := range live {
		merged[k] = v
	}
	for k, v := range stored {
		lv, present := live[k]
		if !liveExtends(lv, present, v) {
			merged[k] = v
		}
	}
	return merged
}

// Effective id of one tool call, call_id before id with any bridge suffix cut, empty when none
func CoalesceToolCallID(callID, id string) string {
	for _, raw := range []string{callID, id} {
		if value := strings.TrimSpace(raw); value != "" {
			head, _, _ := strings.Cut(value, "|")
			if head = strings.TrimSpace(head); head != "" {
				return head
			}
			return value
		}
	}
	return ""
}

// Display dedupe key of a tool calling assistant row built from its stable call ids
type StableToolKey struct {
	Timestamp  float64
	ToolCallID string
	HasCallID  bool
	CallIDs    string
	ToolName   string
	HasName    bool
}

// Stable key of an assistant row whose every tool call names an id, false for any other row
func StableToolKeyOf(role, toolCallsJSON string, timestamp float64, toolCallID, toolName *string) (StableToolKey, bool) {
	if role != "assistant" {
		return StableToolKey{}, false
	}
	if toolCallsJSON == "" {
		toolCallsJSON = "[]"
	}
	var calls []json.RawMessage
	if err := json.Unmarshal([]byte(toolCallsJSON), &calls); err != nil {
		var probe any
		if json.Unmarshal([]byte(toolCallsJSON), &probe) != nil {
			slog.Warn("Failed to deserialize tool_calls, falling back to []")
		}
		return StableToolKey{}, false
	}
	if len(calls) == 0 {
		return StableToolKey{}, false
	}
	ids := make([]string, len(calls))
	for i, raw := range calls {
		var fields map[string]json.RawMessage
		if json.Unmarshal(bytes.TrimSpace(raw), &fields) != nil {
			return StableToolKey{}, false
		}
		ids[i] = CoalesceToolCallID(jsonString(fields["call_id"]), jsonString(fields["id"]))
		if ids[i] == "" {
			return StableToolKey{}, false
		}
	}
	key := StableToolKey{Timestamp: timestamp, CallIDs: fmt.Sprintf("%q", ids)}
	if toolCallID != nil {
		key.ToolCallID, key.HasCallID = *toolCallID, true
	}
	if toolName != nil {
		key.ToolName, key.HasName = *toolName, true
	}
	return key, true
}

// Text of a JSON string value, empty for anything else
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
