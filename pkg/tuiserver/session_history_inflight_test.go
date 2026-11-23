package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Session record of the failed turn retention cases with no turn in flight
func retentionSession() *tuihost.Session {
	return tuihost.NewSession(map[string]any{"image_counter": 0, "cols": 80, "show_reasoning": false,
		"tool_progress_mode": "all", "inflight_turn": nil})
}
