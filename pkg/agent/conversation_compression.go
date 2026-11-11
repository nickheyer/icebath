package agent

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

var syntheticUserPrefixes = []string{
	"[System: Your previous response was truncated", "[System: The previous response was cut off",
	"[System: Your previous tool call", "[Your active task list was preserved across context compression]",
	"[IMPORTANT: Background process ",
}

var syntheticUserFlags = []string{
	"_todo_snapshot_synthetic", "_empty_recovery_synthetic", "_verification_stop_synthetic", "_pre_verify_synthetic",
	"_dropped_toolcall_nudge",
}

// String content or the newline joined text of dict parts, empty for anything else
func compressionMessageText(message map[string]any) string {
	return state.CompressionMessageText(message)
}

// Human intent user row, false for runtime scaffolding, synthetic flags and compaction summaries
func isRealUserMessage(message map[string]any) bool {
	if message == nil || message["role"] != "user" {
		return false
	}
	for _, flag := range syntheticUserFlags {
		if pyrepr.Truthy(message[flag]) {
			return false
		}
	}
	text := pystr.Strip(compressionMessageText(message))
	if text == "" {
		return false
	}
	for _, prefix := range syntheticUserPrefixes {
		if strings.HasPrefix(text, prefix) {
			return false
		}
	}
	return !state.IsSyntheticCompressionUserTurn(message)
}
