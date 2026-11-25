package state

import (
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Live identity keys of a message dict, shared with the agent message metadata
const (
	MessageUID          = "message_uid"
	AbsorbedMessageUIDs = "_absorbed_message_uids"
	ToolCallUIDs        = "_tool_call_uids"
	ToolCallUID         = "_tool_call_uid"
)

// Ordered view of a dict value, live maps in sorted key order, false for a non dict
func asPyObject(v any) (orderedObject, bool) {
	switch x := v.(type) {
	case orderedObject:
		return x, true
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		obj := make(orderedObject, len(keys))
		for i, k := range keys {
			obj[i] = keyValue{k, x[k]}
		}
		return obj, true
	}
	return nil, false
}

// Effective call id of a tool call dict or struct, call_id before id with any bridge suffix cut, empty when none
func CoalesceToolCallID(tc any) string {
	for _, key := range []string{"call_id", "id"} {
		value, _ := ToolCallField(tc, key).(string)
		if value = strings.TrimSpace(value); value != "" {
			head, _, _ := strings.Cut(value, "|")
			if head = strings.TrimSpace(head); head != "" {
				return head
			}
			return value
		}
	}
	return ""
}

// Live key value of a flushed dict, else the column value an import payload carries
func liveOrColumn(msg map[string]any, liveKey, column string) any {
	if v, ok := msg[liveKey]; ok {
		return v
	}
	return msg[column]
}

// Uid list from a live list or its JSON text, unique non empty strings in order
func uidList(value any) []string {
	if s, ok := value.(string); ok {
		value, _ = jsonOr(s, []any{}, "Failed to deserialize a message uid list, falling back to []")
	}
	return UIDList(value)
}

// Tool call uid map from a live dict or its JSON text, keeping non empty uids and non empty uid lists
func uidMap(value any) orderedObject {
	if s, ok := value.(string); ok {
		value, _ = jsonOr(s, orderedObject{}, "Failed to deserialize a tool-call uid map, falling back to {}")
	}
	obj, ok := asPyObject(value)
	out := orderedObject{}
	if !ok {
		return out
	}
	for _, kv := range obj {
		if kv.Key == "" {
			continue
		}
		switch v := kv.Value.(type) {
		case string:
			if v != "" {
				out = append(out, kv)
			}
		case []any:
			valid := len(v) > 0
			for _, u := range v {
				if s, isStr := u.(string); !isStr || s == "" {
					valid = false
				}
			}
			if valid {
				out = append(out, kv)
			}
		}
	}
	return out
}

func toolCallUIDMap(msg map[string]any) orderedObject {
	return uidMap(liveOrColumn(msg, ToolCallUIDs, "tool_call_uids"))
}

// JSON of the absorbed uids column, false when there are none
func absorbedUIDsJSON(msg map[string]any) (string, bool, error) {
	uids := uidList(liveOrColumn(msg, AbsorbedMessageUIDs, "absorbed_message_uids"))
	if len(uids) == 0 {
		return "", false, nil
	}
	text, err := dumpsValue(uids, jsonDefault)
	return text, err == nil, err
}

// Key sorted JSON of the tool call uid map column, false when it is empty
func toolCallUIDsJSON(msg map[string]any) (string, bool, error) {
	uids := toolCallUIDMap(msg)
	if len(uids) == 0 {
		return "", false, nil
	}
	o := jsonDefault
	o.sortKeys = true
	text, err := dumpsValue(uids, o)
	return text, err == nil, err
}

// Tool result uid of a message, false unless it is a non empty string
func toolCallUIDOrNone(msg map[string]any) (string, bool) {
	uid, _ := liveOrColumn(msg, ToolCallUID, "tool_call_uid").(string)
	return uid, uid != ""
}

// Whether a live per occurrence list starts with every stored occurrence
func liveExtends(liveUID, storedUID any) bool {
	stored, ok := storedUID.([]any)
	if !ok {
		stored = []any{storedUID}
	}
	live, ok := liveUID.([]any)
	if !ok {
		return false
	}
	head := live[:min(len(stored), len(live))]
	if len(head) != len(stored) {
		return false
	}
	for i := range stored {
		if !anyutil.Equal(head[i], stored[i]) {
			return false
		}
	}
	return true
}

// Stored uids for calls a rewritten dict still names but lost, so a row addressed rewrite keeps them
func fillMissingToolCallUIDs(row, msg map[string]any) {
	calls, isList := msg["tool_calls"].([]any)
	if !anyutil.IsTrue(row["tool_call_uids"]) || !isList {
		return
	}
	live, ok := asPyObject(msg[ToolCallUIDs])
	if !ok {
		live = orderedObject{}
	}
	named := map[string]bool{}
	for _, tc := range calls {
		named[CoalesceToolCallID(tc)] = true
	}
	merged := append(orderedObject{}, live...)
	added := false
	for _, kv := range uidMap(row["tool_call_uids"]) {
		if _, inLive := live.get(kv.Key); named[kv.Key] && !inLive {
			merged = append(merged, kv)
			added = true
		}
	}
	if added {
		msg[ToolCallUIDs] = merged
	}
}

// Stored uid and tool call uids onto a live dict, the stored value winning unless a live list extends it
func restoreRowIdentity(row, msg map[string]any) {
	if anyutil.IsTrue(row[MessageUID]) {
		msg[MessageUID] = row[MessageUID]
	}
	if anyutil.IsTrue(row["tool_call_uids"]) {
		if stored := uidMap(row["tool_call_uids"]); len(stored) > 0 {
			live, ok := asPyObject(msg[ToolCallUIDs])
			if !ok {
				live = orderedObject{}
			}
			merged := append(orderedObject{}, live...)
			for _, kv := range stored {
				liveUID, _ := live.get(kv.Key)
				if liveExtends(liveUID, kv.Value) {
					continue
				}
				merged = setPyKey(merged, kv.Key, kv.Value)
			}
			msg[ToolCallUIDs] = merged
		}
	}
	if anyutil.IsTrue(row["tool_call_uid"]) {
		msg[ToolCallUID] = row["tool_call_uid"]
	}
}

// Assigns key in an ordered object, keeping the first position like a dict update
func setPyKey(obj orderedObject, key string, value any) orderedObject {
	for i := range obj {
		if obj[i].Key == key {
			obj[i].Value = value
			return obj
		}
	}
	return append(obj, keyValue{key, value})
}

// Stored identity columns onto a restored dict under their live keys, empty columns adding nothing
func restoreIdentityColumns(row, msg map[string]any) {
	restoreRowIdentity(row, msg)
	if anyutil.IsTrue(row["absorbed_message_uids"]) {
		if absorbed := uidList(row["absorbed_message_uids"]); len(absorbed) > 0 {
			msg[AbsorbedMessageUIDs] = absorbed
		}
	}
}

// Display dedupe key of a tool calling assistant row from its stable call ids, false for other rows or incomplete ids
func stableToolKey(row map[string]any) (string, bool, error) {
	if row["role"] != "assistant" {
		return "", false, nil
	}
	raw := row["tool_calls"]
	if !anyutil.IsTrue(raw) {
		raw = "[]"
	}
	calls, err := jsonOr(raw, []any{}, "Failed to deserialize tool_calls, falling back to []")
	if err != nil {
		return "", false, err
	}
	var entries []any
	switch x := calls.(type) {
	case []any:
		entries = x
	case orderedObject:
		for _, kv := range x {
			entries = append(entries, kv.Key)
		}
	case string:
		for _, r := range x {
			entries = append(entries, string(r))
		}
	default:
		if anyutil.IsTrue(x) {
			return "", false, &convError{"TypeError", "a tool key part is " + errchain.Kind(x) + ", expected text or a list"}
		}
	}
	ids := make([]string, len(entries))
	for i, tc := range entries {
		if ids[i] = CoalesceToolCallID(tc); ids[i] == "" {
			return "", false, nil
		}
	}
	if len(ids) == 0 {
		return "", false, nil
	}
	key, err := jsonutil.Encode([]any{"assistant", nil, row["timestamp"], row["tool_call_id"], ids, row["tool_name"]})
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}
