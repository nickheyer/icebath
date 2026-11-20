package telegram

import "testing"

func TestNotificationKwargs(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		metadata map[string]any
		silent   bool
	}{
		{"important", nil, true},
		{"important", map[string]any{"thread_id": "t1"}, true},
		{"important", map[string]any{"notify": true}, false},
		{"important", map[string]any{"thread_id": "t1", "is_approval_prompt": true}, false},
		{"all", nil, false},
		{"all", map[string]any{"is_approval_prompt": true}, false},
	} {
		if got := notificationKwargs(tc.mode, tc.metadata); got != tc.silent {
			t.Fatalf("%s %v: got %v", tc.mode, tc.metadata, got)
		}
	}
}
