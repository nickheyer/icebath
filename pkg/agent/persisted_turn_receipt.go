package agent

import (
	"reflect"

	"github.com/nickheyer/icebath/pkg/state"
)

// Row id of a committed message, nil for an unpersisted row or one without a positive integer id
func CommittedRowID(message any) *int {
	m, ok := message.(map[string]any)
	if !ok {
		return nil
	}
	rowID, isInt := m["_row_id"].(int)
	if !isInt {
		if i64, ok64 := m["_row_id"].(int64); ok64 {
			rowID, isInt = int(i64), true
		}
	}
	if pyTruthy(m[state.DBPersistedMarker]) && isInt && rowID > 0 {
		return &rowID
	}
	return nil
}

// Whether two rows are the same dict object, the identity check of a Python is
func SameRow(before, after any) bool {
	a, b := reflect.ValueOf(before), reflect.ValueOf(after)
	if a.Kind() != reflect.Map || b.Kind() != reflect.Map {
		return false
	}
	return a.Pointer() == b.Pointer()
}

// Committed row addresses of the current turn for message.complete, nil when the turn has no committed rows
func PersistedTurnReceipt(messages any, start *int, history []map[string]any, raw any, status string, compressionUnchanged bool, prefixRowMatches func(before, after any) bool) map[string]any {
	list, ok := messages.([]map[string]any)
	if !ok || start == nil || *start < 0 || *start >= len(list) || list[*start]["role"] != "user" {
		return nil
	}
	tail := list[*start:]
	anchorID := CommittedRowID(tail[0])
	for _, before := range history {
		if SameRow(before, tail[0]) {
			return nil
		}
		if anchorID != nil {
			if id := CommittedRowID(before); id != nil && *id == *anchorID {
				return nil
			}
		}
	}
	var rowIDs []any
	for _, message := range tail {
		if id := CommittedRowID(message); id != nil {
			rowIDs = append(rowIDs, *id)
		}
	}
	if len(rowIDs) == 0 {
		return nil
	}
	receipt := map[string]any{"row_ids": rowIDs, "complete": false}
	if id := CommittedRowID(tail[0]); id != nil {
		receipt["user_row_id"] = *id
	}
	var userRowIDs []any
	users, gap := 0, false
	for _, message := range tail {
		if message["role"] != "user" || message["display_kind"] == "hidden" {
			continue
		}
		users++
		if id := CommittedRowID(message); id != nil {
			userRowIDs = append(userRowIDs, *id)
		} else {
			gap = true
		}
	}
	if users > 0 && !gap {
		receipt["user_row_ids"] = userRowIDs
	}
	last := tail[len(tail)-1]
	if status == "complete" && last["role"] == "assistant" && !pyTruthy(last["tool_calls"]) && reflect.DeepEqual(last["content"], raw) {
		if finalID := CommittedRowID(last); finalID != nil {
			receipt["final_assistant_row_id"] = *finalID
		}
	}
	prefixUnchanged := *start == len(history)
	for i := 0; prefixUnchanged && i < min(len(history), *start); i++ {
		prefixUnchanged = prefixRowMatches(history[i], list[i])
	}
	userRows := 0
	for _, message := range tail {
		if message["role"] == "user" {
			userRows++
		}
	}
	_, hasFinal := receipt["final_assistant_row_id"]
	receipt["complete"] = prefixUnchanged && compressionUnchanged && len(rowIDs) == len(tail) && userRows == 1 && hasFinal
	return receipt
}
