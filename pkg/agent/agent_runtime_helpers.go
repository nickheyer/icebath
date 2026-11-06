package agent

import (
	"reflect"

	"github.com/nickheyer/icebath/pkg/state"
)

// Surviving call ids, result ids, results without a declared call and calls without a result
func classifyToolCallOrphans(messages []map[string]any) (map[string]bool, map[string]bool, []map[string]any, []any) {
	type callEntry struct {
		tc       any
		variants map[string]bool
	}
	var calls []callEntry
	surviving := map[string]bool{}
	for _, msg := range messages {
		if msg["role"] != "assistant" {
			continue
		}
		for _, tc := range anyRows(msg["tool_calls"]) {
			v := state.ToolCallIDVariants(tc)
			if len(v) == 0 {
				continue
			}
			calls = append(calls, callEntry{tc, v})
			for id := range v {
				surviving[id] = true
			}
		}
	}
	type resultEntry struct {
		msg      map[string]any
		variants map[string]bool
	}
	var results []resultEntry
	resultIDs := map[string]bool{}
	for _, msg := range messages {
		if msg["role"] != "tool" {
			continue
		}
		v := state.ToolResultIDVariants(msg["tool_call_id"])
		results = append(results, resultEntry{msg, v})
		for id := range v {
			resultIDs[id] = true
		}
	}
	orphaned := []map[string]any{}
	for _, r := range results {
		if len(r.variants) > 0 && !idSetsIntersect(r.variants, surviving) {
			orphaned = append(orphaned, r.msg)
		}
	}
	missing := []any{}
	for _, c := range calls {
		if !idSetsIntersect(c.variants, resultIDs) {
			missing = append(missing, c.tc)
		}
	}
	return surviving, resultIDs, orphaned, missing
}

// True when the two id sets share a member
func idSetsIntersect(a, b map[string]bool) bool {
	if len(b) < len(a) {
		a, b = b, a
	}
	for id := range a {
		if b[id] {
			return true
		}
	}
	return false
}

// Repairs role alternation in place and moves the flush cursor to the surviving flushed rows
func repairMessageSequenceWithCursor(a *AIAgent, messages *[]*state.Message) (int, error) {
	msgs := *messages
	rows := make([]map[string]any, len(msgs))
	origin := make(map[uintptr]int, len(msgs))
	stamped := map[uintptr]bool{}
	for i, m := range msgs {
		rows[i] = state.MessageToDict(m)
		key := reflect.ValueOf(rows[i]).Pointer()
		origin[key] = i
		if pyTruthy(rows[i][state.DBPersistedMarker]) {
			stamped[key] = true
		}
	}
	cursor := a.lastFlushedDBIdx
	repairs, err := state.RepairMessageSequence(&rows)
	if err != nil || repairs == 0 {
		return repairs, err
	}
	rebuilt := make([]*state.Message, len(rows))
	flushed, lostMarker := 0, false
	for i, row := range rows {
		key := reflect.ValueOf(row).Pointer()
		idx, known := origin[key]
		if !known {
			rebuilt[i] = state.MessageFromDict(row)
			continue
		}
		*msgs[idx] = *state.MessageFromDict(row)
		rebuilt[i] = msgs[idx]
		if cursor > 0 && idx < cursor {
			flushed++
		}
		if stamped[key] && !pyTruthy(row[state.DBPersistedMarker]) {
			lostMarker = true
		}
	}
	*messages = rebuilt
	if lostMarker {
		a.dbFlushScanPrefix = nil
	}
	if cursor > 0 {
		a.lastFlushedDBIdx = flushed
	} else {
		a.lastFlushedDBIdx = min(a.lastFlushedDBIdx, len(rebuilt))
	}
	return repairs, nil
}
