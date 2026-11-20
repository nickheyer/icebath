package tuiserver

import (
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestNoticeIsNotSentForAFinishedBatchAndDoesNotDedupAgainstTheFinalResult(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	queue := tools.DefaultProcessRegistry().CompletionQueue
	for {
		if _, ok := queue.GetNowait(); !ok {
			break
		}
	}
	record := map[string]any{"delegation_id": "deleg_x", "status": "completed", "is_batch": true, "goals": []any{"a", "b", "c"}, "goal": "a",
		"session_key": "sk", "origin_ui_session_id": "ui", "origin_session_id": "", "parent_session_id": "root",
		"dispatched_at": 1.0, "role": "leaf", "model": "m", "context": nil, "toolsets": nil}
	t.Cleanup(tools.SetAsyncDelegationRecordForTests("deleg_x", record))
	tools.PushTaskFailureNotice(&tools.Env{Home: h, Values: &sync.Map{}}, "deleg_x", map[string]any{"task_index": 0, "status": "error"}, 3)
	if evt, ok := queue.GetNowait(); ok {
		t.Fatalf("a finished batch still queued a notice %v", evt)
	}
	notice := map[string]any{"type": "async_delegation", "delegation_id": "deleg_x", "task_failure_notice": true,
		"results": []any{map[string]any{"task_index": 2}}}
	final := map[string]any{"type": "async_delegation", "delegation_id": "deleg_x", "is_batch": true, "results": []any{}}
	if notificationEventDedupKey(notice) == notificationEventDedupKey(final) {
		t.Fatalf("notice and final share the dedup key %q", notificationEventDedupKey(notice))
	}
}
