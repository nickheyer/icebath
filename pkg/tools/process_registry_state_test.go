package tools

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestFormatProcessNotificationMatchesTheRegistryFormatter(t *testing.T) {
	if got := FormatProcessNotification(map[string]any{"type": "watch_disabled", "message": "watch off"}); got != "[IMPORTANT: watch off]" {
		t.Fatalf("watch event got %q", got)
	}
	evt := map[string]any{"session_id": "proc_1", "command": "make", "exit_code": 0, "output": "done"}
	if got, want := FormatProcessNotification(evt), formatProcessNotification(evt); got != want || got == "" {
		t.Fatalf("completion got %q want %q", got, want)
	}
}

func TestProcessAccountingLinesAndOutputTailWrappers(t *testing.T) {
	entry := map[string]any{"orphaned_processes": []any{map[string]any{"session_id": "p1", "command": "sleep 9", "runtime_seconds": 3}}}
	if got, want := ProcessAccountingLines(entry), processAccountingLines(entry); len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Fatalf("ProcessAccountingLines = %v, want %v", got, want)
	}
	s := &ProcessSession{OutputBuffer: "abcdef"}
	if got, want := OutputTail(s, 3), outputTail(s, 3); got != want {
		t.Fatalf("OutputTail = %q, want %q", got, want)
	}
}

func TestNewEventQueueStartsEmpty(t *testing.T) {
	q := NewEventQueue()
	if !q.Empty() || q.Len() != 0 {
		t.Fatalf("new queue holds %d events", q.Len())
	}
	q.Put(map[string]any{"type": "probe"})
	if evt, ok := q.GetNowait(); !ok || evt["type"] != "probe" {
		t.Fatalf("queue returned %v %v", evt, ok)
	}
}

func TestFormatProcessNotificationRendersObjectResults(t *testing.T) {
	evt := map[string]any{
		"type": "async_delegation", "delegation_id": "deleg_obj", "is_batch": true, "goals": []any{"the real task"},
		"results":                []any{pyjson.ObjectOf("task_index", 0, "status", "completed", "summary", "done: the real task", "api_calls", 1, "duration_seconds", 0.1)},
		"total_duration_seconds": 0.5, "role": "leaf", "model": "m",
	}
	text := FormatProcessNotification(evt)
	if !strings.Contains(text, "the real task") || !strings.Contains(text, "done: the real task") || !strings.Contains(text, "TASK 1/1") {
		t.Fatalf("notification %q", text)
	}
}
