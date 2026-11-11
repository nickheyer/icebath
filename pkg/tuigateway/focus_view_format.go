package tuigateway

import (
	"fmt"
	"strings"
)

// Human readable focus status body without ANSI
func FormatFocusStatus(enabled bool, configuredMode any) string {
	mode := strings.ToUpper(NormalizeToolProgressMode(configuredMode, "all"))
	if enabled {
		return fmt.Sprintf("Focus view: ON — only your prompt and the final response.\n  /focus off restores tool progress: %s", mode)
	}
	return fmt.Sprintf("Focus view: OFF — tool progress: %s", mode)
}

// Confirmation line printed when focus view is switched without ANSI
func FormatFocusToggleMessage(enabled bool, configuredMode any) string {
	if enabled {
		return "Focus view enabled — just your prompt and the final response"
	}
	return fmt.Sprintf("Focus view disabled — tool progress: %s", strings.ToUpper(NormalizeToolProgressMode(configuredMode, "all")))
}
