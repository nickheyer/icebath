package gatewayrun

import (
	"reflect"
	"testing"
)

// test_transcript_offset.py test_recursive_queued_followup_keeps_outer_history_offset
func TestRecursiveQueuedFollowupKeepsOuterHistoryOffset(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	historyBeforeChain := []map[string]any{{"role": "user", "content": "Earlier question"}, {"role": "assistant", "content": "Earlier answer"}}
	firstFollowupTurn := []map[string]any{{"role": "user", "content": "First follow-up question"}, {"role": "assistant", "content": "First follow-up answer"}}
	secondFollowupTurn := []map[string]any{{"role": "user", "content": "Second follow-up question"}, {"role": "assistant", "content": "Second follow-up answer"}}
	concat := func(parts ...[]map[string]any) []map[string]any {
		var out []map[string]any
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	currentResult := map[string]any{"history_offset": len(historyBeforeChain), "messages": concat(historyBeforeChain, firstFollowupTurn)}
	followupResult := map[string]any{"history_offset": len(historyBeforeChain) + len(firstFollowupTurn),
		"messages": concat(historyBeforeChain, firstFollowupTurn, secondFollowupTurn)}
	merged := preserveQueuedFollowupHistoryOffset(currentResult, followupResult)
	if merged["history_offset"] != len(historyBeforeChain) {
		t.Fatalf("history_offset = %#v", merged["history_offset"])
	}
	persisted := merged["messages"].([]map[string]any)[merged["history_offset"].(int):]
	if !reflect.DeepEqual(persisted, concat(firstFollowupTurn, secondFollowupTurn)) {
		t.Fatalf("persisted = %#v", persisted)
	}
}
