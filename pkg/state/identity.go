package state

import (
	"encoding/json"
	"sort"
	"strings"
)

// Live identity keys of a message dict, shared with the agent message metadata
const (
	MessageUID          = "message_uid"
	AbsorbedMessageUIDs = "_absorbed_message_uids"
	ToolCallUIDs        = "_tool_call_uids"
	ToolCallUID         = "_tool_call_uid"
)

// Truth value of a column or decoded JSON value under Python bool
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []byte:
		return len(x) > 0
	case int64:
		return x != 0
	case int:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		return strings.TrimLeft(strings.TrimPrefix(x.String(), "-"), "0") != ""
	case []any:
		return len(x) > 0
	case pyObject:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Ordered view of a dict value, live maps in sorted key order, false for a non dict
func asPyObject(v any) (pyObject, bool) {
	switch x := v.(type) {
	case pyObject:
		return x, true
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		obj := make(pyObject, len(keys))
		for i, k := range keys {
			obj[i] = pyKeyValue{k, x[k]}
		}
		return obj, true
	}
	return nil, false
}

// Unique non empty string uids of a list in order, empty for anything else
func UIDList(value any) []string {
	items, _ := value.([]any)
	if strs, ok := value.([]string); ok {
		for _, s := range strs {
			items = append(items, s)
		}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, item := range items {
		if s, ok := item.(string); ok && s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Effective call id of a tool call dict or struct, call_id before id with any bridge suffix cut, empty when none
func CoalesceToolCallID(tc any) string {
	for _, key := range []string{"call_id", "id"} {
		value, _ := ToolCallField(tc, key).(string)
		if value = pyStrip(value); value != "" {
			head, _, _ := strings.Cut(value, "|")
			if head = pyStrip(head); head != "" {
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
func uidMap(value any) pyObject {
	if s, ok := value.(string); ok {
		value, _ = jsonOr(s, pyObject{}, "Failed to deserialize a tool-call uid map, falling back to {}")
	}
	obj, ok := asPyObject(value)
	out := pyObject{}
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

func toolCallUIDMap(msg map[string]any) pyObject {
	return uidMap(liveOrColumn(msg, ToolCallUIDs, "tool_call_uids"))
}

// JSON of the absorbed uids column, false when there are none
func absorbedUIDsJSON(msg map[string]any) (string, bool, error) {
	uids := uidList(liveOrColumn(msg, AbsorbedMessageUIDs, "absorbed_message_uids"))
	if len(uids) == 0 {
		return "", false, nil
	}
	text, err := pyDumps(uids, pyJSONDefault)
	return text, err == nil, err
}

// Key sorted JSON of the tool call uid map column, false when it is empty
func toolCallUIDsJSON(msg map[string]any) (string, bool, error) {
	uids := toolCallUIDMap(msg)
	if len(uids) == 0 {
		return "", false, nil
	}
	o := pyJSONDefault
	o.sortKeys = true
	text, err := pyDumps(uids, o)
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
		if !pyValueEqual(head[i], stored[i]) {
			return false
		}
	}
	return true
}

// Equality of two decoded JSON scalars or containers under Python ==
func pyValueEqual(a, b any) bool {
	ao, aIsObj := asPyObject(a)
	bo, bIsObj := asPyObject(b)
	if aIsObj || bIsObj {
		if !aIsObj || !bIsObj || len(ao) != len(bo) {
			return false
		}
		for _, kv := range ao {
			other, found := bo.get(kv.Key)
			if !found || !pyValueEqual(kv.Value, other) {
				return false
			}
		}
		return true
	}
	al, aIsList := a.([]any)
	bl, bIsList := b.([]any)
	if aIsList || bIsList {
		if !aIsList || !bIsList || len(al) != len(bl) {
			return false
		}
		for i := range al {
			if !pyValueEqual(al[i], bl[i]) {
				return false
			}
		}
		return true
	}
	if as, ok := a.(string); ok {
		bs, ok := b.(string)
		return ok && as == bs
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	af, aErr := pyFloatOf(a)
	bf, bErr := pyFloatOf(b)
	if aErr == nil && bErr == nil {
		ai, aIntErr := pyIntOf(a)
		bi, bIntErr := pyIntOf(b)
		if _, aIsFloat := a.(float64); !aIsFloat && aIntErr == nil {
			if _, bIsFloat := b.(float64); !bIsFloat && bIntErr == nil {
				return ai.Cmp(bi) == 0
			}
		}
		return af == bf
	}
	return a == b
}

// Stored uids for calls a rewritten dict still names but lost, so a row addressed rewrite keeps them
func fillMissingToolCallUIDs(row, msg map[string]any) {
	calls, isList := msg["tool_calls"].([]any)
	if !pyTruthy(row["tool_call_uids"]) || !isList {
		return
	}
	live, ok := asPyObject(msg[ToolCallUIDs])
	if !ok {
		live = pyObject{}
	}
	named := map[string]bool{}
	for _, tc := range calls {
		named[CoalesceToolCallID(tc)] = true
	}
	merged := append(pyObject{}, live...)
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
	if pyTruthy(row[MessageUID]) {
		msg[MessageUID] = row[MessageUID]
	}
	if pyTruthy(row["tool_call_uids"]) {
		if stored := uidMap(row["tool_call_uids"]); len(stored) > 0 {
			live, ok := asPyObject(msg[ToolCallUIDs])
			if !ok {
				live = pyObject{}
			}
			merged := append(pyObject{}, live...)
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
	if pyTruthy(row["tool_call_uid"]) {
		msg[ToolCallUID] = row["tool_call_uid"]
	}
}

// Assigns key in an ordered object, keeping the first position like a dict update
func setPyKey(obj pyObject, key string, value any) pyObject {
	for i := range obj {
		if obj[i].Key == key {
			obj[i].Value = value
			return obj
		}
	}
	return append(obj, pyKeyValue{key, value})
}

// Stored identity columns onto a restored dict under their live keys, empty columns adding nothing
func restoreIdentityColumns(row, msg map[string]any) {
	restoreRowIdentity(row, msg)
	if pyTruthy(row["absorbed_message_uids"]) {
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
	if !pyTruthy(raw) {
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
	case pyObject:
		for _, kv := range x {
			entries = append(entries, kv.Key)
		}
	case string:
		for _, r := range x {
			entries = append(entries, string(r))
		}
	default:
		if pyTruthy(x) {
			return "", false, &pyConvError{"TypeError", "object is not iterable"}
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
	reprIDs := make([]string, len(ids))
	for i, id := range ids {
		reprIDs[i] = pyRepr(id)
	}
	tuple := "(" + strings.Join(reprIDs, ", ")
	if len(ids) == 1 {
		tuple += ","
	}
	tuple += ")"
	return "('assistant', None, " + pyReprValue(row["timestamp"]) + ", " + pyReprValue(row["tool_call_id"]) + ", " + tuple + ", " + pyReprValue(row["tool_name"]) + ")", true, nil
}
