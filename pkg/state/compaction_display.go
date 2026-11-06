package state

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

var originalVisibleSQL = "COALESCE(" + sqlJSONExtract("original.display_metadata", "$."+ModelOnlyKey) + ", 0) = 0"

// SQL filter dropping alias rows that are a later copy of an archived tool result
func notALaterToolCopySQL(alias string) string {
	return " AND NOT (" + alias + ".role = 'tool' AND " + alias + ".message_uid IS NOT NULL AND EXISTS (" +
		"SELECT 1 FROM messages AS original WHERE original.session_id = " + alias + ".session_id " +
		"AND original.timestamp = " + alias + ".timestamp AND original.message_uid = " + alias + ".message_uid " +
		"AND original.role = 'tool' AND original.active = 0 AND original.compacted = 1 " +
		"AND +original.display_order < " + alias + ".display_order AND " + originalVisibleSQL + "))"
}

// Ids of display rows that repeat an archived tool result held in an earlier display slot
func laterToolCopyIDs(rows []*pyjson.Object) map[int64]bool {
	var tools []*pyjson.Object
	for _, row := range rows {
		if rowValue(row, "role") == "tool" {
			tools = append(tools, row)
		}
	}
	out := map[int64]bool{}
	if len(tools) == 0 || !rowHas(tools[0], "compacted") {
		return out
	}
	var candidates []*pyjson.Object
	for _, row := range tools {
		if pyTruthy(rowValue(row, "active")) || pyTruthy(rowValue(row, "compacted")) {
			candidates = append(candidates, row)
		}
	}
	if len(candidates) == 0 || !rowHas(candidates[0], "display_order") {
		return out
	}
	owner := func(row *pyjson.Object) string {
		return pyReprValue(rowValue(row, "session_id")) + "\x00" + pyReprValue(rowValue(row, "message_uid")) + "\x00" + pyReprValue(rowValue(row, "timestamp"))
	}
	firstSlot := map[string]int64{}
	for _, row := range candidates {
		slot, ok := rowInt(row, "display_order")
		if pyTruthy(rowValue(row, "message_uid")) && ok && !pyTruthy(rowValue(row, "active")) && !isModelOnlyRow(row) {
			if cur, seen := firstSlot[owner(row)]; !seen || slot < cur {
				firstSlot[owner(row)] = slot
			}
		}
	}
	for _, row := range candidates {
		slot, ok := rowInt(row, "display_order")
		if !pyTruthy(rowValue(row, "message_uid")) || !ok {
			continue
		}
		if first, seen := firstSlot[owner(row)]; seen && first < slot {
			id, _ := rowInt(row, "id")
			out[id] = true
		}
	}
	return out
}

// Whether a row carries the column at all
func rowHas(row *pyjson.Object, key string) bool {
	_, ok := row.Get(key)
	return ok
}

// Integer column of a row, false when absent or NULL
func rowInt(row *pyjson.Object, key string) (int64, bool) {
	switch v := rowValue(row, key).(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Whether a row is model only, the twin of the display visible SQL filter
func isModelOnlyRow(row *pyjson.Object) bool {
	meta := decodeDisplayMetadata(rowValue(row, "display_metadata"))
	v, _ := mappingGet(meta, ModelOnlyKey)
	return pyTruthy(v)
}
