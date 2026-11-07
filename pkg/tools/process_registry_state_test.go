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
