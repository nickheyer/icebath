package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// True when a request runs inline on the caller instead of an abandonable worker, for cron turns and delegated children
func ShouldUseDirectAPICall(ctx context.Context, provider, platform string, mode APIMode) bool {
	if provider == "moa" {
		return false
	}
	if platform == "cron" {
		return mode == ModeChatCompletions || mode == ModeCodexResponses
	}
	if mode != ModeChatCompletions {
		return false
	}
	if tools.IsDelegatedChildContext(ctx) {
		return true
	}
	return platform == "subagent"
}

// Inline predicate seam of the reconnect loop
var directAPICallFor = ShouldUseDirectAPICall
