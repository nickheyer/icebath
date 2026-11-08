package tools

import (
	"testing"
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
